"""MLflow tracing: one trace per session, tagged so it can be found again.

`mlflow.autolog()` covers the parts of the run that go through LangChain — every
`ChatOpenAI` call becomes a `CHAT_MODEL` span carrying the model id, the token counts and
the latency, and the LangGraph nodes and the `ToolNode` become spans of their own. It does
not reach three things that matter here, and those carry `@mlflow.trace` explicitly:

- **the session itself**, which is not a LangChain object at all — the root span is opened
  in the orchestrator, and it is what the tags hang off;
- **the sandbox**, where a fifth of the wall clock goes and none of it is a model call;
- **the guardrails**, which are pure functions and would otherwise be invisible; they are
  the whole reason `evals.safety_scorer` can be a pure function of a stored trace.

The tools and the retriever are traced explicitly *as well as* by autolog. The duplication
is deliberate: the trajectory extractor needs a tool span whose name is the registered tool
name and whose attributes carry `gen_ai.tool.name`, and inferring that from whatever autolog
happened to name the span is the kind of coupling that breaks on a library upgrade.
`evals.trajectory` keeps only the outermost tool span of any nested chain, so the extra span
does not double-count a call.

**Tags.** `request_origin` separates traffic the taught way — `api` for programmatic calls,
`ui` for the playground, `batch` for an eval or attack run — so a filter can exclude eval
runs from a latency dashboard. `eval_case_id` is a join key, not a filter: it is only set on
`batch` traces, where the case set is bounded and known in advance, which is the one place
where a high-cardinality tag is safe.

Disabling is global and real: `configure()` calls `mlflow.tracing.disable()` when tracing is
off, so the `@mlflow.trace` decorators applied at import time cost nothing. The test suite
runs that way.
"""

from __future__ import annotations

import logging
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any

import mlflow
from mlflow.entities import SpanType

logger = logging.getLogger(__name__)

ROOT_SPAN = "problem_helper.session"

_configured = False


@dataclass(slots=True, frozen=True)
class TraceContext:
    """Who asked, and on behalf of which eval case.

    `request_origin` is one of `api` / `ui` / `batch`. `eval_case_id` is set only on batch
    runs; on interactive traffic it stays `None` so the tag never appears.
    """

    request_origin: str = "api"
    eval_case_id: str | None = None

    def tags(self, session_id: str) -> dict[str, str]:
        tags = {"request_origin": self.request_origin, "session_id": session_id}
        if self.eval_case_id:
            tags["eval_case_id"] = self.eval_case_id
        return tags


def configure(
    *, enabled: bool, tracking_uri: str, experiment: str
) -> bool:
    """Points MLflow at a store and turns autolog on. Idempotent; safe to call per process."""
    global _configured
    if _configured:
        return enabled
    _configured = True

    if not enabled:
        mlflow.tracing.disable()
        logger.info("tracing disabled")
        return False

    # Explicit, not implied: `disable()` is process-wide, so a process that switched
    # tracing off earlier (the test suite does, at import time) has to switch it back on.
    mlflow.tracing.enable()
    mlflow.set_tracking_uri(tracking_uri)
    mlflow.set_experiment(experiment)
    # The generic switch is what the course asks for; the LangChain flavour is named as
    # well so an upgrade that changes which integrations `autolog()` picks up by default
    # cannot silently take the LLM and tool spans away.
    mlflow.autolog(silent=True)
    mlflow.langchain.autolog(silent=True)
    logger.info("tracing enabled: %s, experiment %s", tracking_uri, experiment)
    return True


def reset() -> None:
    """Lets a test reconfigure; the service configures once at startup."""
    global _configured
    _configured = False


@contextmanager
def session_span(session_id: str, context: TraceContext, **inputs: Any) -> Iterator[Any]:
    """The root span of one agent invocation, with the tags that make it findable."""
    with mlflow.start_span(name=ROOT_SPAN, span_type=SpanType.AGENT) as span:
        span.set_inputs({"session_id": session_id, **inputs})
        mlflow.update_current_trace(tags=context.tags(session_id))
        yield span


@contextmanager
def guardrail_span(name: str, **attributes: Any) -> Iterator[Any]:
    """A `GUARDRAIL` span around one layer's decision.

    The verdict goes on the span outputs rather than into a log line, because the safety
    scorer reads it back off the stored trace and never sees the process that produced it.
    """
    with mlflow.start_span(name=f"guardrail.{name}", span_type=SpanType.GUARDRAIL) as span:
        if attributes:
            span.set_inputs(attributes)
        yield span


def set_outputs(span: Any, payload: Any) -> None:
    """Writes a span's outputs, tolerating a no-op span when tracing is off."""
    if span is not None:
        span.set_outputs(payload)


def current_trace_id() -> str | None:
    span = mlflow.get_current_active_span()
    return span.trace_id if span is not None else None
