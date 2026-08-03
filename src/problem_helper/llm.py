"""Thin wrapper around an OpenAI-compatible API with structured output.

Degradation order:
1. `response_format=json_schema` (strict) — the main path;
2. if the provider/model cannot do json_schema (BadRequest), the schema moves into the
   prompt and that mode is remembered for the model for the lifetime of the client;
3. the answer is stripped of markdown fencing and validated by pydantic; on failure the
   model gets one more shot, with its own broken answer shown back to it.
"""

from __future__ import annotations

import json
import logging
from typing import Any, Protocol, TypeVar

from openai import APIError, AsyncOpenAI, BadRequestError
from pydantic import BaseModel, ValidationError

from .config import Settings

logger = logging.getLogger(__name__)

T = TypeVar("T", bound=BaseModel)


class LLMError(RuntimeError):
    """Could not obtain a valid structured answer."""


class LLMProtocol(Protocol):
    """The interface a fake replaces in tests."""

    async def structured(
        self, *, model: str, system: str, user: str, schema: type[T]
    ) -> T: ...


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
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._client = AsyncOpenAI(
            base_url=settings.llm_base_url,
            api_key=settings.llm_api_key,
            timeout=settings.llm_timeout_sec,
            max_retries=settings.llm_max_retries,
        )
        self._no_json_schema: set[str] = set()

    async def close(self) -> None:
        await self._client.close()

    async def structured(
        self, *, model: str, system: str, user: str, schema: type[T]
    ) -> T:
        use_native = model not in self._no_json_schema
        messages: list[dict[str, Any]] = [
            {"role": "system", "content": system},
            {"role": "user", "content": user if use_native else _with_schema(user, schema)},
        ]

        last_error = ""
        parse_attempts = 2
        while parse_attempts > 0:
            try:
                content = await self._call(model, messages, schema, native=use_native)
            except BadRequestError:
                if not use_native:
                    raise
                logger.warning("model %s rejects json_schema, falling back to prompt", model)
                self._no_json_schema.add(model)
                use_native = False
                messages[1]["content"] = _with_schema(user, schema)
                continue
            except APIError as exc:
                raise LLMError(f"{model}: provider error: {exc}") from exc

            try:
                return schema.model_validate_json(extract_json(content))
            except (ValidationError, json.JSONDecodeError) as exc:
                last_error = str(exc)
                parse_attempts -= 1
                logger.warning(
                    "model %s returned invalid JSON, %s attempt(s) left", model, parse_attempts
                )
                messages.append({"role": "assistant", "content": content})
                messages.append(
                    {
                        "role": "user",
                        "content": (
                            "Your answer failed validation:\n"
                            f"{last_error}\n\n"
                            "Return ONLY valid JSON matching the schema, "
                            "with no markdown and no comments."
                        ),
                    }
                )

        raise LLMError(f"{model}: could not obtain valid JSON: {last_error}")

    async def _call(
        self,
        model: str,
        messages: list[dict[str, Any]],
        schema: type[T],
        *,
        native: bool,
    ) -> str:
        kwargs: dict[str, Any] = {}
        if native:
            kwargs["response_format"] = {
                "type": "json_schema",
                "json_schema": {
                    "name": schema.__name__,
                    "strict": True,
                    "schema": strict_schema(schema),
                },
            }

        response = await self._client.chat.completions.create(
            model=model, messages=messages, **kwargs
        )
        if usage := getattr(response, "usage", None):
            logger.info(
                "llm %s: prompt=%s completion=%s",
                model,
                usage.prompt_tokens,
                usage.completion_tokens,
            )
        content = response.choices[0].message.content if response.choices else None
        if not content:
            raise LLMError(f"{model}: empty answer")
        return content


def _with_schema(user: str, schema: type[BaseModel]) -> str:
    return (
        f"{user}\n\n"
        "Answer with exactly one JSON object matching this schema, no markdown wrapper:\n"
        f"{json.dumps(strict_schema(schema), ensure_ascii=False)}"
    )
