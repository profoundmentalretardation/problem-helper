"""LangChain access to an OpenAI-compatible provider: structured output and tool calling.

Both entry points go through `langchain_openai.ChatOpenAI`; the raw `openai` SDK is only
imported for the error types the provider raises.

Structured output degrades in the same order as before:
1. `with_structured_output(..., method="json_schema", strict=True)` — the main path;
2. if the provider/model cannot do json_schema (BadRequest/NotFound) the schema moves into
   the prompt, and that decision is remembered for the model for the lifetime of the client;
3. the answer is stripped of markdown fencing and validated by pydantic; on failure the
   model gets one more shot, with its own broken answer shown back to it.

Tool calling is plain `bind_tools`: the model receives the tool schemas and decides on its
own whether to call any of them — the graph only executes what comes back.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from typing import Any, Protocol, TypeVar

from langchain_core.messages import AIMessage, AnyMessage, HumanMessage, SystemMessage
from langchain_core.tools import BaseTool
from langchain_openai import ChatOpenAI
from openai import APIError, BadRequestError, NotFoundError
from pydantic import BaseModel, ValidationError

from .config import Settings

logger = logging.getLogger(__name__)

T = TypeVar("T", bound=BaseModel)

_UNSUPPORTED = (BadRequestError, NotFoundError)


class LLMError(RuntimeError):
    """Could not obtain a valid structured answer."""


class LLMProtocol(Protocol):
    """The interface a fake replaces in tests."""

    async def structured(
        self,
        *,
        model: str,
        system: str,
        user: str,
        schema: type[T],
        history: Sequence[AnyMessage] = (),
    ) -> T: ...

    async def chat(
        self, *, model: str, messages: Sequence[AnyMessage], tools: Sequence[BaseTool]
    ) -> AIMessage: ...


def strict_schema(model: type[BaseModel]) -> dict[str, Any]:
    """JSON Schema in the shape strict mode accepts (every field required)."""
    schema = model.model_json_schema()
    _harden(schema)
    return schema


def _harden(node: Any) -> None:
    if isinstance(node, dict):
        if node.get("type") == "object" and "properties" in node:
            node["additionalProperties"] = False
            node["required"] = list(node["properties"])
        for value in node.values():
            _harden(value)
    elif isinstance(node, list):
        for value in node:
            _harden(value)


def extract_json(text: str) -> str:
    """Pulls JSON out of an answer: unwraps ``` fencing or takes the first {...} block."""
    cleaned = text.strip()
    if cleaned.startswith("```"):
        cleaned = cleaned.split("\n", 1)[-1]
        if cleaned.rstrip().endswith("```"):
            cleaned = cleaned.rstrip()[: -len("```")]
        cleaned = cleaned.strip()
    if cleaned.startswith("{"):
        return cleaned
    start, end = cleaned.find("{"), cleaned.rfind("}")
    if start != -1 and end > start:
        return cleaned[start : end + 1]
    return cleaned


class LLMClient:
    """One `ChatOpenAI` per model id, built lazily and reused."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._models: dict[str, ChatOpenAI] = {}
        self._no_json_schema: set[str] = set()

    def model(self, model: str) -> ChatOpenAI:
        if model not in self._models:
            self._models[model] = ChatOpenAI(
                model=model,
                base_url=self._settings.llm_base_url,
                api_key=self._settings.llm_api_key or "unset",
                timeout=self._settings.llm_timeout_sec,
                max_retries=self._settings.llm_max_retries,
                temperature=self._settings.llm_temperature,
            )
        return self._models[model]

    async def close(self) -> None:
        self._models.clear()

    # ------------------------------------------------------------------ #

    async def structured(
        self,
        *,
        model: str,
        system: str,
        user: str,
        schema: type[T],
        history: Sequence[AnyMessage] = (),
    ) -> T:
        """A JSON answer validated by `schema`.

        `history` carries an earlier exchange — the hint agent passes its tool-calling
        conversation here so the final answer is written with the tool results in context.
        """
        if model not in self._no_json_schema:
            try:
                return await self._native(model, system, user, schema, history)
            except _UNSUPPORTED:
                logger.warning("model %s rejects json_schema, falling back to prompt", model)
                self._no_json_schema.add(model)
            except (ValidationError, json.JSONDecodeError) as exc:
                logger.warning(
                    "model %s broke the strict schema (%s), retrying by prompt", model, exc
                )
        return await self._by_prompt(model, system, user, schema, history)

    async def chat(
        self, *, model: str, messages: Sequence[AnyMessage], tools: Sequence[BaseTool]
    ) -> AIMessage:
        """One turn of a tool-calling conversation; the model may answer or call tools."""
        runnable = self.model(model)
        if tools:
            runnable = runnable.bind_tools(list(tools))
        try:
            answer = await runnable.ainvoke(list(messages))
        except APIError as exc:
            raise LLMError(f"{model}: provider error: {exc}") from exc
        if not isinstance(answer, AIMessage):  # pragma: no cover - provider contract
            raise LLMError(f"{model}: expected an assistant message, got {type(answer).__name__}")
        _log_usage(model, answer)
        return answer

    # ------------------------------------------------------------------ #

    async def _native(
        self,
        model: str,
        system: str,
        user: str,
        schema: type[T],
        history: Sequence[AnyMessage],
    ) -> T:
        runnable = self.model(model).with_structured_output(
            schema, method="json_schema", strict=True
        )
        messages = [SystemMessage(system), *history, HumanMessage(user)]
        try:
            return await runnable.ainvoke(messages)
        except _UNSUPPORTED:
            raise
        except APIError as exc:
            raise LLMError(f"{model}: provider error: {exc}") from exc

    async def _by_prompt(
        self,
        model: str,
        system: str,
        user: str,
        schema: type[T],
        history: Sequence[AnyMessage] = (),
    ) -> T:
        messages: list[AnyMessage] = [
            SystemMessage(system),
            *history,
            HumanMessage(_with_schema(user, schema)),
        ]
        last_error = ""
        for remaining in (1, 0):
            try:
                answer = await self.model(model).ainvoke(messages)
            except APIError as exc:
                raise LLMError(f"{model}: provider error: {exc}") from exc

            _log_usage(model, answer)
            content = answer.text if isinstance(answer.content, list) else str(answer.content)
            if not content.strip():
                raise LLMError(f"{model}: empty answer")

            try:
                return schema.model_validate_json(extract_json(content))
            except (ValidationError, json.JSONDecodeError) as exc:
                last_error = str(exc)
                logger.warning(
                    "model %s returned invalid JSON, %s attempt(s) left", model, remaining
                )
                messages.append(AIMessage(content))
                messages.append(
                    HumanMessage(
                        "Your answer failed validation:\n"
                        f"{last_error}\n\n"
                        "Return ONLY valid JSON matching the schema, "
                        "with no markdown and no comments."
                    )
                )

        raise LLMError(f"{model}: could not obtain valid JSON: {last_error}")


def _log_usage(model: str, message: AIMessage) -> None:
    usage = getattr(message, "usage_metadata", None)
    if usage:
        logger.info(
            "llm %s: prompt=%s completion=%s",
            model,
            usage.get("input_tokens"),
            usage.get("output_tokens"),
        )


def _with_schema(user: str, schema: type[BaseModel]) -> str:
    return (
        f"{user}\n\n"
        "Answer with exactly one JSON object matching this schema, no markdown wrapper:\n"
        f"{json.dumps(strict_schema(schema), ensure_ascii=False)}"
    )
