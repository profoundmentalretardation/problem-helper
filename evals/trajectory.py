"""The adapters: one MLflow trace → the inputs the scorers already take.

This is the twenty-line layer the whole of Part 1 stands on, and it is the reason the
tracing went in first. Nothing here knows how a session is run; it reads a stored trace, so
the same function serves a live run, a replay of last week's traffic, and the eval suite.

Two adapters, because there are two kinds of scorer:

- `tool_calls(trace)` → the ordered `list[ToolCall]` the trajectory metrics consume.
- `rag_inputs(trace)` → the `(question, answer, contexts)` triple the HW2 judge takes, with
  no change to the judge whatsoever. `evals.judge.Judge` is imported and called as it
  stands; if wiring a scorer to a trace required editing the scorer, the adapter would be
  doing too little.

**Why the outermost tool span wins.** A tool call shows up in the trace twice: once from
`mlflow.autolog()`, which wraps the LangChain tool invocation, and once from the
`@mlflow.trace` in `problem_helper.tools`, which pins `gen_ai.tool.name`. They nest, parent
and child, around the same call. Counting both would double every trajectory, so a tool span
that has another tool span above it is dropped — the surviving span is the one the framework
opened, which is also the one that exists when only autolog is on.

**Arguments, never results.** A trajectory is what the agent *decided to do*. Folding the
tool's output into it would make the metric partly a measure of the corpus, and a trajectory
that changes when the corpus is re-chunked is not measuring the agent.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

TOOL = "TOOL"
RETRIEVER = "RETRIEVER"
AGENT = "AGENT"
GUARDRAIL = "GUARDRAIL"

TOOL_NAME_ATTR = "gen_ai.tool.name"


@dataclass(frozen=True, slots=True)
class ToolCall:
    """One decision the agent made: which tool, with which arguments."""

    tool: str
    arguments: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict:
        return {"tool": self.tool, "arguments": self.arguments}

    def __str__(self) -> str:
        args = ", ".join(f"{k}={v!r}" for k, v in sorted(self.arguments.items()))
        return f"{self.tool}({args})"


@dataclass(slots=True)
class RagInputs:
    """What the HW2 generation scorers take, lifted off a trace."""

    question: str
    answer: str
    contexts: list[str]

    @property
    def retrieved(self) -> bool:
        """Whether the agent used the corpus at all — the scorers are undefined if it did not."""
        return bool(self.contexts)


# --------------------------------------------------------------------------- #
# Trajectory
# --------------------------------------------------------------------------- #


def tool_calls(trace: Any) -> list[ToolCall]:
    """The agent's tool calls, in the order it made them."""
    spans = list(_spans(trace))
    by_id = {span.span_id: span for span in spans}
    tools = [span for span in spans if _type(span) == TOOL]
    outermost = [span for span in tools if _tool_ancestor(span, by_id) is None]
    outermost.sort(key=lambda span: span.start_time_ns)
    return [
        ToolCall(tool=_tool_name(span, by_id, tools), arguments=_arguments(span))
        for span in outermost
    ]


def _tool_ancestor(span: Any, by_id: dict[str, Any]) -> Any | None:
    parent = by_id.get(span.parent_id) if span.parent_id else None
    while parent is not None:
        if _type(parent) == TOOL:
            return parent
        parent = by_id.get(parent.parent_id) if parent.parent_id else None
    return None


def _tool_name(span: Any, by_id: dict[str, Any], tools: list[Any]) -> str:
    """`gen_ai.tool.name` where it exists, on the span or on the duplicate underneath it."""
    named = span.attributes.get(TOOL_NAME_ATTR)
    if named:
        return str(named)
    for other in tools:
        if _tool_ancestor(other, by_id) is span and other.attributes.get(TOOL_NAME_ATTR):
            return str(other.attributes[TOOL_NAME_ATTR])
    return str(span.name)


def _arguments(span: Any) -> dict[str, Any]:
    inputs = span.inputs
    if isinstance(inputs, dict):
        # LangChain hands a tool its arguments under `input` when it takes a single value.
        return dict(inputs.get("input", inputs) if _is_wrapper(inputs) else inputs)
    return {}


def _is_wrapper(inputs: dict) -> bool:
    return set(inputs) == {"input"} and isinstance(inputs["input"], dict)


# --------------------------------------------------------------------------- #
# RAG scorer inputs
# --------------------------------------------------------------------------- #


def rag_inputs(trace: Any) -> RagInputs:
    """`(question, answer, contexts)` for the judge, off the root span and the tool results.

    The *question* is the student's task, because that is the information need the hint has
    to address. The *contexts* are the passages the agent actually saw, taken from the tool
    outputs rather than from the retriever, so grounding is judged against the text that
    reached the model — the excerpt, not the full chunk behind it.
    """
    root = _root(trace)
    inputs = _payload(root.inputs if root is not None else None)
    outputs = _payload(root.outputs if root is not None else None)
    return RagInputs(
        question=str(inputs.get("task", "")),
        answer=str(outputs.get("hint") or ""),
        contexts=contexts(trace),
    )


def contexts(trace: Any) -> list[str]:
    """Every passage the tools handed back, in the order the agent received them."""
    out: list[str] = []
    seen: set[str] = set()
    for span in sorted(
        (s for s in _spans(trace) if _type(s) == TOOL), key=lambda s: s.start_time_ns
    ):
        for passage in _passages(span.outputs):
            if passage not in seen:
                seen.add(passage)
                out.append(passage)
    return out


def _passages(outputs: Any) -> list[str]:
    payload = _decode(outputs)
    items = payload if isinstance(payload, list) else [payload]
    passages = []
    for item in items:
        if not isinstance(item, dict):
            continue
        if "excerpt" in item:
            head = " — ".join(str(item[k]) for k in ("title", "heading") if item.get(k))
            passages.append(f"{head}\n{item['excerpt']}" if head else str(item["excerpt"]))
        elif "body" in item:  # a full material, from get_learning_material
            passages.append(f"{item.get('title', '')}\n{item['body']}".strip())
    return passages


def _decode(outputs: Any) -> Any:
    """Tool output, whichever of the two tool spans it came off.

    Ours carries the parsed value; the autolog span carries the `ToolMessage`, so the JSON
    is one level down under `content`.
    """
    if isinstance(outputs, dict) and "content" in outputs:
        outputs = outputs["content"]
    if isinstance(outputs, str):
        try:
            return json.loads(outputs)
        except json.JSONDecodeError:
            return None
    return outputs


# --------------------------------------------------------------------------- #
# Guardrails and shared helpers
# --------------------------------------------------------------------------- #


def guardrail_events(trace: Any) -> list[dict]:
    """Every guardrail decision on the trace, oldest first. Used by the safety scorer."""
    events = []
    for span in sorted(
        (s for s in _spans(trace) if _type(s) == GUARDRAIL), key=lambda s: s.start_time_ns
    ):
        payload = _payload(span.outputs)
        events.append({"layer": span.name.removeprefix("guardrail."), **payload})
    return events


def outcome(trace: Any) -> dict:
    """The root span's outputs: the verdict, the hint and the counters."""
    root = _root(trace)
    return _payload(root.outputs if root is not None else None)


def tags(trace: Any) -> dict[str, str]:
    return dict(getattr(trace.info, "tags", {}) or {})


def _spans(trace: Any) -> list[Any]:
    return list(getattr(trace.data, "spans", []) or [])


def _root(trace: Any) -> Any | None:
    for span in _spans(trace):
        if _type(span) == AGENT and not span.parent_id:
            return span
    return next((span for span in _spans(trace) if not span.parent_id), None)


def _type(span: Any) -> str:
    return str(getattr(span, "span_type", "") or "")


def _payload(value: Any) -> dict:
    value = _decode(value)
    return value if isinstance(value, dict) else {}
