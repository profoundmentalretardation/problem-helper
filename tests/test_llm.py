import httpx
import pytest
from openai import BadRequestError
from pydantic import BaseModel

from problem_helper.config import Settings
from problem_helper.llm import LLMClient, LLMError, extract_json, strict_schema


class Answer(BaseModel):
    value: int
    note: str


class FakeCompletions:
    """Returns canned answers and records the calls."""

    def __init__(self, responses):
        self._responses = list(responses)
        self.calls = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        item = self._responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return _completion(item)


def _completion(content: str):
    message = type("Msg", (), {"content": content})()
    choice = type("Choice", (), {"message": message})()
    return type("Resp", (), {"choices": [choice], "usage": None})()


def make_client(responses) -> tuple[LLMClient, FakeCompletions]:
    client = LLMClient(Settings(llm_api_key="test"))
    fake = FakeCompletions(responses)
    client._client = type("C", (), {"chat": type("Chat", (), {"completions": fake})()})()
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


async def test_structured_parses_valid_response():
    client, fake = make_client(['{"value": 7, "note": "ok"}'])

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result == Answer(value=7, note="ok")
    assert fake.calls[0]["response_format"]["json_schema"]["strict"] is True


async def test_structured_retries_after_invalid_json():
    client, fake = make_client(["not json", '{"value": 1, "note": "n"}'])

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result.value == 1
    assert len(fake.calls) == 2
    # the second request carries the model's broken answer plus the fix-it instruction
    assert fake.calls[1]["messages"][-1]["content"].startswith("Your answer failed validation")


async def test_structured_gives_up_after_two_bad_answers():
    client, _ = make_client(["garbage", "more garbage"])

    with pytest.raises(LLMError, match="valid JSON"):
        await client.structured(model="m", system="s", user="u", schema=Answer)


async def test_falls_back_to_prompt_schema_when_provider_rejects():
    client, fake = make_client([bad_request(), '{"value": 3, "note": "n"}'])

    result = await client.structured(model="m", system="s", user="u", schema=Answer)

    assert result.value == 3
    assert "response_format" not in fake.calls[1]
    assert "JSON object matching this schema" in fake.calls[1]["messages"][1]["content"]
    assert "m" in client._no_json_schema
