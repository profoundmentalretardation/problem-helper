"""Driving the real pipeline for a batch run, and finding the trace it produced.

Shared by the agent eval and the attack suite, because both want the same thing: run the
actual service against a case and then read what happened off the trace.

**It calls the orchestrator, not the HTTP API.** The eval is not testing FastAPI, and going
through HTTP would mean either running a server or mocking one; the orchestrator is the
entry point of the background task and is exactly the unit an end-to-end scenario exercises.
It also lets the run supply its own `TraceContext` — `request_origin="batch"` plus the case
id — which an HTTP client could not do for `eval_case_id` without a header nobody else uses.

**Finding the trace.** Not by "the last active trace": runs go concurrently, and last-active
is per-thread state that would hand back somebody else's run. The session id is on the trace
as a tag, is unique per run by construction, and `search_traces` filters on it — which is the
same lookup a human would do in the UI, and the reason the tag is written in the first place.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Any
from uuid import uuid4

import mlflow

from problem_helper.config import Settings
from problem_helper.db import Database
from problem_helper.llm import LLMClient
from problem_helper.orchestrator import process_session
from problem_helper.schemas import SolveRequest
from problem_helper.tracing import TraceContext

logger = logging.getLogger(__name__)


@dataclass(slots=True)
class Run:
    """One execution of one case."""

    case_id: str
    attempt: int
    session_id: str
    record: dict
    trace: Any | None = None

    @property
    def trace_id(self) -> str | None:
        return self.trace.info.trace_id if self.trace is not None else None


async def run_case(
    case_id: str,
    request: SolveRequest,
    *,
    attempt: int,
    db: Database,
    llm: LLMClient,
    settings: Settings,
    semaphore: asyncio.Semaphore,
) -> Run:
    """Runs one case once and returns the stored session row.

    Exceptions are not caught here. `_run` inside the orchestrator already turns a crash
    into an `internal_error` session, which is a legitimate outcome for a scenario to
    record — a run that dies is a run that failed the scenario, not a hole in the table.
    """
    session_id = uuid4().hex
    async with semaphore:
        await db.create_session(session_id, request.model_dump())
        await process_session(
            session_id,
            request,
            db=db,
            llm=llm,
            settings=settings,
            trace=TraceContext(request_origin="batch", eval_case_id=case_id),
        )
    record = await db.get_session(session_id) or {}
    logger.info(
        "  %-24s run %s → %s/%s",
        case_id,
        attempt,
        record.get("status"),
        record.get("error_code") or (record.get("result") or {}).get("outcome"),
    )
    return Run(case_id=case_id, attempt=attempt, session_id=session_id, record=record)


def attach_traces(runs: list[Run]) -> int:
    """Looks every run's trace up by its session tag. Returns how many were found.

    Called once after the whole batch: trace export is asynchronous, so flushing per run
    would serialise the batch behind the exporter for no benefit.
    """
    mlflow.flush_trace_async_logging()
    found = 0
    for run in runs:
        traces = mlflow.search_traces(
            filter_string=f"tags.session_id = '{run.session_id}'",
            max_results=1,
            return_type="list",
        )
        if traces:
            run.trace = traces[0]
            found += 1
        else:
            logger.warning("no trace found for %s run %s", run.case_id, run.attempt)
    return found


def eval_settings(temperature: float, **overrides) -> Settings:
    """Service settings for a batch run.

    The temperature is the whole point: every notebook in the course pins 0.0 to stay
    reproducible, and three runs at 0.0 are three copies of one run — `pass^3` would be
    identically equal to `pass@1` and requirement 6 would be unsatisfiable. It is raised
    here and nowhere else; the service still answers at 0.0, so a student who re-opens a
    session gets the advice they were given the first time.
    """
    return Settings(llm_temperature=temperature, **overrides)
