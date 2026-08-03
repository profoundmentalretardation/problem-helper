import httpx
import pytest
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage
from openai import BadRequestError
from pydantic import BaseModel

from problem_helper.config import Settings
from problem_helper.llm import LLMClient, LLMError, extract_json, strict_schema
from problem_helper.tools import TOOLS


class Answer(BaseModel):
    value: int
    note: str


class FakeChatModel:
    """Stands in for `ChatOpenAI`: serves canned answers and records what it was sent."""

    def __init__(self, responses, structured_responses=None):
        self._responses = list(responses)
        self._structured = list(structured_responses or [])
        self.calls: list[dict] = []

    # --- the bits of the ChatOpenAI surface the client uses ---------------- #

    def with_structured_output(self, schema, **kwargs):
        return _Runnable(self, "structured", kwargs, self._structured)

    def bind_tools(self, tools):
        return _Runnable(self, "tools", {"tools": tools}, self._responses)

    async def ainvoke(self, messages):
        return await _Runnable(self, "plain", {}, self._responses).ainvoke(messages)


class _Runnable:
    def __init__(self, parent, kind, kwargs, queue):
        self._parent = parent
        self._kind = kind
        self._kwargs = kwargs
        self._queue = queue

    async def ainvoke(self, messages):
        self._parent.calls.append({"kind": self._kind, "messages": messages, **self._kwargs})
        item = self._queue.pop(0)
        if isinstance(item, Exception):
            raise item
        return AIMessage(item) if isinstance(item, str) else item


def make_client(responses=(), structured_responses=()) -> tuple[LLMClient, FakeChatModel]:
    client = LLMClient(Settings(llm_api_key="test"))
    fake = FakeChatModel(responses, structured_responses)
    client._models["m"] = fake
    return client, fake


def bad_request() -> BadRequestError:
    request = httpx.Request("POST", "https://example.test/v1/chat/completions")
    return BadRequestError(
        "json_schema not supported",
        response=httpx.Response(400, request=request),
        body=None,
    )


def test_strict_schema_forces_required_and_no_extra():
    schema = strict_schema(Answer)

    assert schema["additionalProperties"] is False
    assert sorted(schema["required"]) == ["note", "value"]


def test_extract_json_strips_markdown_fence():
    assert extract_json('```json\n{"a": 1}\n```') == '{"a": 1}'
    assert extract_json('Here you go: {"a": 1} done') == '{"a": 1}'
    assert extract_json('{"a": 1}') == '{"a": 1}'


async def test_structured_uses_strict_json_schema():
    client, fake = make_client(structured_responses=[Answer(value=7, note="ok")])

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result == Answer(value=7, note="ok")
    call = fake.calls[0]
    assert call["method"] == "json_schema"
    assert call["strict"] is True
    assert [type(m) for m in call["messages"]] == [SystemMessage, HumanMessage]


async def test_structured_passes_the_earlier_conversation_through():
    client, fake = make_client(structured_responses=[Answer(value=1, note="n")])
    history = [HumanMessage("earlier"), AIMessage("answer")]

    await client.structured(model="m", system="s", user="u", schema=Answer, history=history)

    assert [m.content for m in fake.calls[0]["messages"]] == ["s", "earlier", "answer", "u"]


async def test_falls_back_to_prompt_schema_when_provider_rejects():
    client, fake = make_client(
        responses=['{"value": 3, "note": "n"}'], structured_responses=[bad_request()]
    )

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result.value == 3
    assert fake.calls[1]["kind"] == "plain"
    assert "JSON object matching this schema" in fake.calls[1]["messages"][-1].content
    assert "m" in client._no_json_schema

    # the choice is remembered: the second call does not try json_schema again
    fake._responses.append('{"value": 4, "note": "n"}')
    await client.structured(model="m", system="s", user="u", schema=Answer)
    assert [c["kind"] for c in fake.calls] == ["structured", "plain", "plain"]


async def test_retries_once_after_invalid_json():
    client, fake = make_client(
        responses=["not json", '{"value": 1, "note": "n"}'], structured_responses=[bad_request()]
    )

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result.value == 1
    assert fake.calls[-1]["messages"][-1].content.startswith("Your answer failed validation")


async def test_gives_up_after_two_bad_answers():
    client, _ = make_client(
        responses=["garbage", "more garbage"], structured_responses=[bad_request()]
    )

    with pytest.raises(LLMError, match="valid JSON"):
        await client.structured(model="m", system="s", user="u", schema=Answer)


async def test_chat_binds_the_tools_and_returns_the_answer():
    client, fake = make_client(responses=[AIMessage("no tools needed")])

    answer = await client.chat(model="m", messages=[HumanMessage("hi")], tools=TOOLS)

    assert answer.content == "no tools needed"
    assert {t.name for t in fake.calls[0]["tools"]} == {t.name for t in TOOLS}
