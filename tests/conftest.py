from dataclasses import dataclass, field

import pytest
from langchain_core.messages import AIMessage, AnyMessage
from pydantic import BaseModel

from problem_helper import materials, retrieval, tracing
from problem_helper.retrieval import Chunk, Hit
from problem_helper.sandbox import TestOutcome, TestReport
from problem_helper.schemas import TestCase

# The `@mlflow.trace` decorators are applied at import time, so switching tracing off has
# to happen before any of them runs. `configure` is idempotent, so the app's own call
# during `create_app`'s lifespan is a no-op afterwards — which is what keeps the suite from
# writing an `mlruns/` tree into the repository.
tracing.configure(enabled=False, tracking_uri="", experiment="")


@dataclass
class FakeCall:
    model: str
    system: str
    user: str
    schema: type[BaseModel]
    history: list[AnyMessage]


@dataclass
class FakeChatCall:
    model: str
    messages: list[AnyMessage]
    tools: list


@dataclass
class FakeLLM:
    """Serves canned answers keyed by schema type; the last answer repeats.

    `chat` follows the same rule over `turns`; with no script it answers without tool
    calls, which sends the hint agent straight to writing.
    """

    script: dict[type[BaseModel], list[BaseModel]]
    turns: list[AIMessage] = field(default_factory=list)
    calls: list[FakeCall] = field(default_factory=list)
    chat_calls: list[FakeChatCall] = field(default_factory=list)

    async def structured(self, *, model, system, user, schema, history=()):
        self.calls.append(
            FakeCall(
                model=model, system=system, user=user, schema=schema, history=list(history)
            )
        )
        queue = self.script[schema]
        return queue.pop(0) if len(queue) > 1 else queue[0]

    async def chat(self, *, model, messages, tools):
        self.chat_calls.append(
            FakeChatCall(model=model, messages=list(messages), tools=list(tools))
        )
        if not self.turns:
            return AIMessage("no tools needed")
        answer = self.turns.pop(0) if len(self.turns) > 1 else self.turns[0]
        # a fresh id on every turn, otherwise add_messages would replace the earlier copy
        return answer.model_copy(update={"id": None})

    def users_for(self, schema: type[BaseModel]) -> list[str]:
        return [c.user for c in self.calls if c.schema is schema]


def tool_turn(name: str, call_id: str = "call-1", **args) -> AIMessage:
    """An assistant turn that asks for one tool call."""
    return AIMessage(
        content="",
        tool_calls=[{"name": name, "args": args, "id": call_id, "type": "tool_call"}],
    )


def report(passed: bool, total: int = 2) -> TestReport:
    """A synthetic report, built without spawning processes."""
    return TestReport(
        outcomes=[
            TestOutcome(
                index=i,
                passed=passed,
                input="",
                expected_output="7",
                actual_output="7" if passed else "-1",
                stderr="",
                exit_code=0,
                timed_out=False,
                duration_ms=1,
            )
            for i in range(total)
        ]
    )


def sample_tests() -> list[TestCase]:
    return [TestCase(input="3 4\n", expected_output="7")]


def chunk(material_id: str, section: int = 0, heading: str = "Pitfalls") -> Chunk:
    """A chunk carrying real catalog metadata, without building an index."""
    material = materials.get(material_id)
    assert material is not None, material_id
    return Chunk(
        id=f"{material_id}#{section}.0",
        material_id=material_id,
        title=material.title,
        topic=material.topic,
        heading=heading,
        text=f"{heading}: excerpt of {material.title}.",
        tags=tuple(material.tags),
    )


@dataclass
class StubRetriever:
    """Stands in for `RetrievalService` so unit tests never load the ONNX models.

    The real pipeline is covered by the pure-python tests over BM25/RRF/chunking and by the
    evaluation harness, which runs it against the whole corpus.
    """

    chunks: list[Chunk]
    queries: list[tuple[str, int]] = field(default_factory=list)

    def search(self, query: str, *, k: int | None = None, rerank: bool | None = None):
        self.queries.append((query, k or len(self.chunks)))
        return [
            Hit(chunk=c, score=1.0 - i / 10, stage="rerank", rrf_rank=i + 1)
            for i, c in enumerate(self.chunks[: k or len(self.chunks)])
        ]


@pytest.fixture(autouse=True)
def stub_retriever():
    """Every test runs against the stub: no ONNX model is loaded by the unit suite.

    The real pipeline is covered by the pure-python tests over chunking/BM25/RRF and end to
    end by the evaluation harness, which runs it against the whole corpus.
    """
    retriever = StubRetriever(
        [
            chunk("algo-prefix-sums", 0, "Building the array"),
            chunk("algo-binary-search", 2, "Insertion points instead of membership"),
            chunk("algo-parity-filters", 3, "Summing the wrong thing"),
        ]
    )
    retrieval.set_retriever(retriever)
    yield retriever
    retrieval.set_retriever(None)
