"""The trace adapters, on hand-built spans and once against a real trace.

The hand-built cases pin the rules the extractor is supposed to follow — order, the nested
duplicate, arguments without results. The integration test at the bottom is the one that
would actually notice an MLflow upgrade changing the span shape underneath us, so it runs
the real graph with tracing switched on and reads the trajectory back off the stored trace.
"""

from dataclasses import dataclass, field
from typing import Any

import mlflow
import pytest
from conftest import FakeLLM, report, sample_tests, tool_turn
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver

from evals import trajectory
from problem_helper import tracing
from problem_helper.graph import build_graph, initial_state
from problem_helper.schemas import FixResult, HintResult, Mistake, ValidationResult


@dataclass
class Span:
    """The subset of an MLflow span the adapters read."""

    name: str
    span_type: str
    span_id: str
    parent_id: str | None = None
    start_time_ns: int = 0
    attributes: dict = field(default_factory=dict)
    inputs: Any = None
    outputs: Any = None


@dataclass
class Info:
    trace_id: str = "tr-1"
    tags: dict = field(default_factory=dict)


@dataclass
class Data:
    spans: list = field(default_factory=list)


@dataclass
class Trace:
    data: Data
    info: Info = field(default_factory=Info)


def trace_of(*spans: Span, tags: dict | None = None) -> Trace:
    return Trace(data=Data(spans=list(spans)), info=Info(tags=tags or {}))


# --------------------------------------------------------------------------- #
# Trajectory
# --------------------------------------------------------------------------- #


def test_tool_calls_are_ordered_by_start_time():
    trace = trace_of(
        Span("root", "AGENT", "a"),
        Span("get_learning_material", "TOOL", "c", "a", 200, inputs={"material_id": "x"}),
        Span("search_corpus", "TOOL", "b", "a", 100, inputs={"query": "why", "k": 3}),
    )

    calls = trajectory.tool_calls(trace)

    assert [c.tool for c in calls] == ["search_corpus", "get_learning_material"]
    assert calls[0].arguments == {"query": "why", "k": 3}


def test_a_nested_duplicate_counts_once():
    """Autolog and our own decorator both open a tool span around the same call."""
    trace = trace_of(
        Span("root", "AGENT", "a"),
        Span("search_corpus", "TOOL", "b", "a", 100, inputs={"query": "why", "k": 3},
             attributes={"tool_call_id": "call-1"}),
        Span("search_corpus", "TOOL", "c", "b", 101, inputs={"query": "why", "k": 3},
             attributes={"gen_ai.tool.name": "search_corpus"}),
    )

    calls = trajectory.tool_calls(trace)

    assert len(calls) == 1
    assert calls[0].tool == "search_corpus"


def test_the_name_comes_from_the_gen_ai_attribute_when_present():
    trace = trace_of(
        Span("root", "AGENT", "a"),
        Span("RunnableSequence", "TOOL", "b", "a", 100, inputs={"query": "q"},
             attributes={"gen_ai.tool.name": "search_corpus"}),
    )

    assert trajectory.tool_calls(trace)[0].tool == "search_corpus"


def test_results_never_enter_the_trajectory():
    trace = trace_of(
        Span("root", "AGENT", "a"),
        Span("search_corpus", "TOOL", "b", "a", 100,
             inputs={"query": "q"}, outputs='[{"material_id": "algo-sets"}]'),
    )

    call = trajectory.tool_calls(trace)[0]

    assert call.arguments == {"query": "q"}
    assert "algo-sets" not in str(call.as_dict())


def test_no_tool_span_is_an_empty_trajectory():
    assert trajectory.tool_calls(trace_of(Span("root", "AGENT", "a"))) == []


# --------------------------------------------------------------------------- #
# RAG inputs, guardrails, tags
# --------------------------------------------------------------------------- #


def test_rag_inputs_are_lifted_off_the_root_span_and_the_tool_results():
    trace = trace_of(
        Span("root", "AGENT", "a", inputs={"task": "Sum two numbers"},
             outputs={"hint": "look at line 2"}),
        Span(
            "search_corpus", "TOOL", "b", "a", 100,
            inputs={"query": "q"},
            outputs='[{"title": "Parity", "heading": "Pitfalls", "excerpt": "x % 2 == 0"}]',
        ),
    )

    inputs = trajectory.rag_inputs(trace)

    assert inputs.question == "Sum two numbers"
    assert inputs.answer == "look at line 2"
    assert inputs.contexts == ["Parity — Pitfalls\nx % 2 == 0"]
    assert inputs.retrieved


def test_a_tool_message_payload_is_unwrapped():
    """The autolog span carries the ToolMessage, so the JSON is one level down."""
    trace = trace_of(
        Span("root", "AGENT", "a", inputs={"task": "t"}, outputs={"hint": "h"}),
        Span("search_corpus", "TOOL", "b", "a", 100, inputs={"query": "q"},
             outputs={"content": '[{"title": "T", "heading": "H", "excerpt": "E"}]'}),
    )

    assert trajectory.rag_inputs(trace).contexts == ["T — H\nE"]


def test_a_run_without_retrieval_reports_no_contexts():
    trace = trace_of(Span("root", "AGENT", "a", inputs={"task": "t"}, outputs={"hint": "h"}))

    assert not trajectory.rag_inputs(trace).retrieved


def test_guardrail_events_come_back_in_order():
    trace = trace_of(
        Span("root", "AGENT", "a"),
        Span("guardrail.input_filter", "GUARDRAIL", "b", "a", 100, outputs={"blocked": False}),
        Span("guardrail.code_shield", "GUARDRAIL", "c", "a", 200, outputs={"allowed": True}),
    )

    events = trajectory.guardrail_events(trace)

    assert [e["layer"] for e in events] == ["input_filter", "code_shield"]
    assert events[0]["blocked"] is False


def test_tags_are_read_off_the_trace_info():
    trace = trace_of(Span("root", "AGENT", "a"), tags={"request_origin": "batch"})

    assert trajectory.tags(trace)["request_origin"] == "batch"


# --------------------------------------------------------------------------- #
# Against a real trace
# --------------------------------------------------------------------------- #


@pytest.fixture
def traced(tmp_path):
    """Switches tracing on for one test and puts it back afterwards."""
    tracing.reset()
    tracing.configure(
        enabled=True,
        tracking_uri=f"sqlite:///{tmp_path}/mlflow.db",
        experiment="test-trajectory",
    )
    yield
    mlflow.flush_trace_async_logging()
    tracing.reset()
    tracing.configure(enabled=False, tracking_uri="", experiment="")


async def test_extracts_the_trajectory_of_a_real_run(traced):
    """The shape the adapters actually have to survive: whatever MLflow emits today."""
    llm = FakeLLM(
        script={
            FixResult: [
                FixResult(
                    mistakes=[Mistake(title="op", detail="minus", line=2)],
                    fixed_code="print(7)",
                    summary="use +",
                )
            ],
            HintResult: [HintResult(hint="Look at line 2.", related_material_ids=[])],
            ValidationResult: [ValidationResult(approved=True, issues=[])],
        },
        turns=[
            tool_turn("search_corpus", query="adding two numbers", k=3),
            tool_turn("get_learning_material", call_id="call-2", material_id="algo-loop-bounds"),
            AIMessage("ready"),
        ],
    )
    reports = iter([report(False), report(True)])
    graph = build_graph(
        llm=llm,
        runner=lambda code: _ready(next(reports)),
        fixer_model="f",
        hint_model="h",
        validator_model="v",
        checkpointer=InMemorySaver(),
    )

    with tracing.session_span("sess-x", tracing.TraceContext("batch", "case-1"), task="Sum"):
        await graph.ainvoke(
            initial_state(
                task="Sum",
                student_code="print(1)",
                tests=sample_tests(),
                max_fix_attempts=2,
                max_hint_attempts=2,
            ),
            {"configurable": {"thread_id": "sess-x"}},
        )
    mlflow.flush_trace_async_logging()

    found = mlflow.search_traces(
        filter_string="tags.session_id = 'sess-x'", max_results=1, return_type="list"
    )
    assert found, "the run produced no trace"
    calls = trajectory.tool_calls(found[0])

    assert [c.tool for c in calls] == ["search_corpus", "get_learning_material"]
    assert calls[0].arguments == {"query": "adding two numbers", "k": 3}
    assert calls[1].arguments == {"material_id": "algo-loop-bounds"}
    assert trajectory.tags(found[0])["eval_case_id"] == "case-1"
    assert [e["layer"] for e in trajectory.guardrail_events(found[0])] == [
        "input_filter",
        "code_shield",
        "code_shield",
        "output_filter",
    ]


async def _ready(value):
    return value
