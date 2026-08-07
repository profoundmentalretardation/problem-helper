"""Scorers that take a stored trace and write their verdicts back onto it.

This is the join between the three parts of the homework. Part 2 produced traces; Part 1
defined trajectory metrics; HW2 defined generation metrics. Nothing here re-implements any
of them — `evals.judge.Judge` and `evals.agent_metrics` are imported and called exactly as
they stand. All this module contributes is the adapter layer plus `mlflow.log_feedback`, so
a metric computed offline lands on the run that produced it and can be read back from the
trace UI, from `search_traces`, or by the next scorer along.

Three scorers, and they are deliberately different in kind:

- `score_trajectory` is pure and free. Given a trace and a case's expectation it produces
  the four Part 1 numbers. It needs no model, which is why it can run over every trace.
- `score_rag` runs the HW2 judge over `(question, answer, contexts)` lifted off the trace.
  It costs model calls and only makes sense on traces where the agent actually retrieved —
  faithfulness against an empty context set is not a low score, it is an undefined one, and
  the runner counts those separately rather than averaging zeros into the table.
- `score_safety` lives in `evals.safety_scorer`; it is also pure, and it reads the guardrail
  spans rather than the tool spans.

**The scorer bodies do not change.** `Judge.faithfulness` takes `question`, `answer` and
`contexts`; the adapter's whole job is to produce those three from a span tree. If wiring a
scorer to a trace had required editing the scorer, the adapter would be doing too little —
that is the test this module is written to pass.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any

import mlflow
from mlflow.entities import AssessmentSource, AssessmentSourceType

from . import agent_metrics, trajectory
from .agent_metrics import Expectation
from .judge import Judge

logger = logging.getLogger(__name__)

CODE_SOURCE = AssessmentSource(
    source_type=AssessmentSourceType.CODE, source_id="evals.trace_scorers"
)

TRAJECTORY_METRICS = (
    "tool_selection_accuracy",
    "tool_parameter_accuracy",
    "trajectory_precision",
    "trajectory_recall",
    "goal_completion",
)

RAG_METRICS = ("faithfulness", "answer_relevance", "context_precision", "context_recall")


@dataclass(slots=True)
class Feedback:
    """One metric value with the reason it came out that way."""

    name: str
    value: float | bool | None
    rationale: str = ""

    def as_dict(self) -> dict:
        return {"name": self.name, "value": self.value, "rationale": self.rationale}


# --------------------------------------------------------------------------- #
# Part 1 metrics over a trace
# --------------------------------------------------------------------------- #


def score_trajectory(trace: Any, expectation: Expectation) -> tuple[list[Feedback], dict]:
    """The four Part 1 metrics for one traced run. Pure — no model, no network."""
    calls = trajectory.tool_calls(trace)
    outcome = trajectory.outcome(trace)
    cited = list(outcome.get("materials") or [])

    selection = agent_metrics.tool_selection_accuracy(calls, expectation)
    parameters, parameter_failures = agent_metrics.tool_parameter_accuracy(calls, expectation)
    precision, recall, matched_against = agent_metrics.trajectory_scores(calls, expectation)
    completion, completion_reasons = agent_metrics.goal_completion(
        outcome, expectation, cited_materials=cited
    )

    trail = " → ".join(str(call) for call in calls) or "<no tool call>"
    feedbacks = [
        Feedback(
            "tool_selection_accuracy",
            selection,
            f"used [{trail}]; acceptable: {expectation.alternatives}",
        ),
        Feedback(
            "tool_parameter_accuracy",
            parameters,
            "; ".join(parameter_failures) or "every constrained argument was usable",
        ),
        Feedback("trajectory_precision", precision, f"scored against {matched_against}"),
        Feedback("trajectory_recall", recall, f"scored against {matched_against}"),
        Feedback(
            "goal_completion",
            completion,
            "; ".join(completion_reasons) or "reached the expected outcome",
        ),
    ]
    detail = {
        "trajectory": [call.as_dict() for call in calls],
        "outcome": outcome,
        "matched_against": matched_against,
        "parameter_failures": parameter_failures,
        "completion_reasons": completion_reasons,
        **{f.name: f.value for f in feedbacks},
    }
    return feedbacks, detail


def passed(detail: dict) -> bool:
    """What counts as a passing run of a scenario.

    Both halves have to hold: the agent has to have *done* an acceptable thing
    (`tool_selection_accuracy`) and to have *ended* where the case says it should
    (`goal_completion`). Outcome alone would pass an agent that produced the right hint
    after four redundant searches; trajectory alone would pass an agent that researched
    beautifully and then wrote nonsense.
    """
    return detail.get("tool_selection_accuracy") == 1.0 and detail.get("goal_completion") == 1.0


# --------------------------------------------------------------------------- #
# HW2 generation metrics over a trace
# --------------------------------------------------------------------------- #


async def score_rag(trace: Any, judge: Judge) -> tuple[list[Feedback], dict]:
    """The HW2 judge over the question, the hint and the passages the agent actually saw.

    Returns empty feedback for a run that never retrieved: three of the four metrics are
    about the relationship between an answer and its context, and there is no context.
    """
    inputs = trajectory.rag_inputs(trace)
    if not inputs.retrieved or not inputs.answer:
        return [], {"skipped": "the run retrieved nothing" if not inputs.retrieved else "no hint"}

    faithfulness = await judge.faithfulness(
        question=inputs.question, answer=inputs.answer, contexts=inputs.contexts
    )
    relevance = await judge.answer_relevance(question=inputs.question, answer=inputs.answer)
    precision = await judge.context_precision(
        question=inputs.question, contexts=inputs.contexts
    )
    scores = {
        "faithfulness": faithfulness,
        "answer_relevance": relevance,
        "context_precision": precision,
    }
    feedbacks = [
        Feedback(f"rag_{name}", payload["score"], _rationale(name, payload))
        for name, payload in scores.items()
    ]
    return feedbacks, {"contexts": len(inputs.contexts), **{k: v for k, v in scores.items()}}


def _rationale(name: str, payload: dict) -> str:
    if name == "faithfulness":
        missing = payload.get("unsupported") or []
        return f"{payload['claims']} claim(s), unsupported: {missing or 'none'}"
    if name == "answer_relevance":
        return f"{payload['requirements']} requirement(s), evasive={payload['evasive']}"
    return f"{payload['useful']}/{payload['of']} passages useful"


# --------------------------------------------------------------------------- #
# Writing back
# --------------------------------------------------------------------------- #


def log(trace_id: str, feedbacks: list[Feedback]) -> int:
    """Writes every non-null verdict onto the trace. Returns how many were written.

    A `None` value is skipped rather than written as zero — the distinction between "the
    metric says 0" and "the metric does not apply to this run" is the one thing an
    aggregate cannot recover afterwards.
    """
    written = 0
    for feedback in feedbacks:
        if feedback.value is None:
            continue
        mlflow.log_feedback(
            trace_id=trace_id,
            name=feedback.name,
            value=feedback.value,
            rationale=feedback.rationale or None,
            source=CODE_SOURCE,
        )
        written += 1
    return written
