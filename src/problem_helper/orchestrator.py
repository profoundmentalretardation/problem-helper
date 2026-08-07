"""Background session driver: runs the LangGraph pipeline and persists what it emits.

`process_session` and `resume_session` are the only entry points of the background task
and the only places that write session status. Both funnel into `_run`, which swallows
exceptions into `internal_error`, so a crashed session never stays stuck in `running`.

The graph itself knows nothing about storage: this module streams its updates and turns
them into stage changes, attempt rows and the final result.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from typing import Any

from langchain_core.messages import AIMessage
from langgraph.checkpoint.base import BaseCheckpointSaver

from . import sandbox, tracing
from .config import Settings
from .db import Database
from .graph import GuardConfig, build_graph, initial_state
from .llm import LLMProtocol
from .sandbox import SandboxUnavailable, TestReport
from .schemas import (
    ErrorCode,
    MaterialRef,
    MistakeOut,
    Outcome,
    SessionResult,
    SessionStage,
    SessionStatus,
    SolveRequest,
)
from .tracing import TraceContext

logger = logging.getLogger(__name__)


def make_runner(
    request: SolveRequest, settings: Settings
) -> Callable[[str], Awaitable[TestReport]]:
    async def runner(code: str) -> TestReport:
        return await sandbox.run_tests(
            code,
            request.tests,
            backend=settings.sandbox_backend,
            image=settings.sandbox_image,
            timeout_sec=settings.sandbox_timeout_sec,
            memory_mb=settings.sandbox_memory_mb,
            max_output_bytes=settings.sandbox_max_output_bytes,
        )

    return runner


async def process_session(
    session_id: str,
    request: SolveRequest,
    *,
    db: Database,
    llm: LLMProtocol,
    settings: Settings,
    checkpointer: BaseCheckpointSaver | None = None,
    trace: TraceContext | None = None,
) -> None:
    """Runs a fresh session from the start."""
    await _run(session_id, request, db=db, llm=llm, settings=settings,
               checkpointer=checkpointer, resume=False, trace=trace)


async def resume_session(
    session_id: str,
    request: SolveRequest,
    *,
    db: Database,
    llm: LLMProtocol,
    settings: Settings,
    checkpointer: BaseCheckpointSaver,
    trace: TraceContext | None = None,
) -> None:
    """Continues a session from its last checkpoint — nothing already done is paid for twice."""
    await _run(session_id, request, db=db, llm=llm, settings=settings,
               checkpointer=checkpointer, resume=True, trace=trace)


async def _run(
    session_id: str,
    request: SolveRequest,
    *,
    db: Database,
    llm: LLMProtocol,
    settings: Settings,
    checkpointer: BaseCheckpointSaver | None,
    resume: bool,
    trace: TraceContext | None = None,
) -> None:
    try:
        await _drive(
            session_id,
            request,
            db=db,
            llm=llm,
            settings=settings,
            checkpointer=checkpointer,
            resume=resume,
            trace=trace or TraceContext(),
        )
    except SandboxUnavailable as exc:
        # Not an internal error and not the student's fault: the isolation the service
        # promises is not there, so the session stops rather than running anything.
        logger.error("session %s: sandbox unavailable: %s", session_id, exc)
        await db.finish_failure(session_id, ErrorCode.sandbox_unavailable, str(exc))
    except Exception as exc:
        logger.exception("session %s crashed", session_id)
        await db.finish_failure(
            session_id, ErrorCode.internal_error, f"{type(exc).__name__}: {exc}"
        )


async def _drive(
    session_id: str,
    request: SolveRequest,
    *,
    db: Database,
    llm: LLMProtocol,
    settings: Settings,
    checkpointer: BaseCheckpointSaver | None,
    resume: bool,
    trace: TraceContext,
) -> None:
    graph = build_graph(
        llm=llm,
        runner=make_runner(request, settings),
        fixer_model=settings.fixer_model,
        hint_model=settings.hint_model,
        validator_model=settings.validator_model,
        checkpointer=checkpointer,
        guards=GuardConfig.from_settings(settings),
    )
    config = {"configurable": {"thread_id": session_id}}
    # `None` tells LangGraph to pick the run up where the checkpoint left off.
    payload = (
        None
        if resume
        else initial_state(
            task=request.task,
            student_code=request.code,
            tests=request.tests,
            max_fix_attempts=request.max_fix_attempts or settings.max_fix_attempts,
            max_hint_attempts=request.max_hint_attempts or settings.max_hint_attempts,
        )
    )

    await db.set_stage(session_id, SessionStatus.running, SessionStage.running_tests)

    values: dict[str, Any] = {}
    tool_calls: list[dict] = []
    # The root span of the trace: every LLM, tool, retrieval and guardrail span produced
    # below hangs off it, and the tags are what make this run findable afterwards.
    with tracing.session_span(
        session_id, trace, task=request.task, resumed=resume
    ) as span:
        async for mode, chunk in graph.astream(
            payload, config, stream_mode=["updates", "values"]
        ):
            if mode == "values":
                values = chunk
                continue
            for node, update in chunk.items():
                if isinstance(update, dict):
                    await _persist(session_id, node, update, db=db, tool_calls=tool_calls)

        tracing.set_outputs(
            span,
            {
                "outcome": values.get("outcome"),
                "error_code": values.get("error_code"),
                "hint": values.get("hint"),
                "materials": [ref.id for ref in values.get("materials", [])],
                "fix_attempts_used": values.get("fix_round"),
                "hint_attempts_used": values.get("hint_round"),
            },
        )

    await _finish(session_id, values, tool_calls, db=db)


async def _persist(
    session_id: str,
    node: str,
    update: dict,
    *,
    db: Database,
    tool_calls: list[dict],
) -> None:
    stage = update.get("stage")
    if stage is not None and stage != SessionStage.done:
        await db.set_stage(session_id, SessionStatus.running, stage)
    for record in update.get("fix_attempts", []):
        await db.add_attempt(session_id, "fix", record["attempt"], record)
    for record in update.get("hint_attempts", []):
        await db.add_attempt(session_id, "hint", record["attempt"], record)
    if node == "research":
        tool_calls.extend(_tool_calls(update.get("research", [])))


async def _finish(
    session_id: str, values: dict[str, Any], tool_calls: list[dict], *, db: Database
) -> None:
    internals: dict[str, Any] = {"baseline_tests": values.get("baseline")}
    if tool_calls:
        internals["tool_calls"] = tool_calls
    if values.get("guardrails"):
        internals["guardrails"] = values["guardrails"]

    if values.get("error_code") == ErrorCode.unsafe_input:
        # Refused before anything ran, so there is no baseline and no attempt to report.
        await db.finish_failure(
            session_id,
            ErrorCode.unsafe_input,
            values.get("error_message", ""),
            {"guardrails": values.get("guardrails", [])},
        )
        return

    if values.get("outcome") == Outcome.already_correct:
        logger.info("session %s: the student's code already passes the tests", session_id)
        await db.finish_success(
            session_id,
            SessionResult(
                outcome=Outcome.already_correct,
                tests_total=values.get("tests_total", 0),
                tests_passed_before=values.get("tests_passed_before", 0),
            ).model_dump(),
            internals,
        )
        return

    fix_attempts = values.get("fix_attempts", [])
    if values.get("error_code") == ErrorCode.fix_failed:
        internals["fix_attempts"] = fix_attempts
        await db.finish_failure(
            session_id, ErrorCode.fix_failed, values.get("error_message", ""), internals
        )
        return

    internals |= {
        "fixed_code": values.get("fixed_code"),
        "fix_summary": values.get("fix_summary"),
        "diff": values.get("diff"),
        "fix_attempts_used": values.get("fix_round"),
    }

    if values.get("error_code") == ErrorCode.hint_rejected:
        internals["rejected_hints"] = values.get("hint_attempts", [])
        await db.finish_failure(
            session_id, ErrorCode.hint_rejected, values.get("error_message", ""), internals
        )
        return

    if values.get("outcome") != Outcome.hint_ready:
        # The graph stopped without a verdict — an interrupted run that was never resumed.
        await db.finish_failure(
            session_id,
            ErrorCode.internal_error,
            "the pipeline stopped without a verdict",
            internals,
        )
        return

    internals["hint_attempts_used"] = values.get("hint_round")
    materials = [MaterialRef.model_validate(ref) for ref in values.get("materials", [])]
    await db.finish_success(
        session_id,
        SessionResult(
            outcome=Outcome.hint_ready,
            hint=values.get("hint"),
            mistakes=[
                MistakeOut(title=m.title, detail=m.detail, line=m.line or None)
                for m in values.get("mistakes", [])
            ],
            materials=materials,
            tests_total=values.get("tests_total", 0),
            tests_passed_before=values.get("tests_passed_before", 0),
        ).model_dump(),
        internals,
    )


def _tool_calls(messages: list) -> list[dict]:
    return [
        {"name": call["name"], "args": call["args"]}
        for message in messages
        if isinstance(message, AIMessage)
        for call in message.tool_calls
    ]
