"""Background session pipeline: tests → fix loop → diff → hint loop."""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable

from . import codediff, sandbox
from .config import Settings
from .db import Database
from .fixer import FixAttempt, run_fix_loop
from .hinter import HintAttempt, run_hint_loop
from .llm import LLMProtocol
from .sandbox import TestReport
from .schemas import (
    ErrorCode,
    MistakeOut,
    Outcome,
    SessionResult,
    SessionStage,
    SessionStatus,
    SolveRequest,
)

logger = logging.getLogger(__name__)


def make_runner(
    request: SolveRequest, settings: Settings
) -> Callable[[str], Awaitable[TestReport]]:
    async def runner(code: str) -> TestReport:
        return await sandbox.run_tests(
            code,
            request.tests,
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
) -> None:
    """The only entry point for the background task. Any failure lands in the database."""
    try:
        await _process(session_id, request, db=db, llm=llm, settings=settings)
    except Exception as exc:
        logger.exception("session %s crashed", session_id)
        await db.finish_failure(
            session_id, ErrorCode.internal_error, f"{type(exc).__name__}: {exc}"
        )


async def _process(
    session_id: str,
    request: SolveRequest,
    *,
    db: Database,
    llm: LLMProtocol,
    settings: Settings,
) -> None:
    runner = make_runner(request, settings)

    await db.set_stage(session_id, SessionStatus.running, SessionStage.running_tests)
    baseline = await runner(request.code)
    internals: dict = {"baseline_tests": baseline.as_dict()}

    if baseline.all_passed:
        logger.info("session %s: the student's code already passes the tests", session_id)
        await db.finish_success(
            session_id,
            SessionResult(
                outcome=Outcome.already_correct,
                tests_total=baseline.total,
                tests_passed_before=baseline.passed_count,
            ).model_dump(),
            internals,
        )
        return

    # --- loop 1: repair --------------------------------------------------- #
    await db.set_stage(session_id, SessionStatus.running, SessionStage.fixing)

    async def save_fix(attempt: FixAttempt) -> None:
        await db.add_attempt(session_id, "fix", attempt.number, attempt.as_dict())

    fix_outcome = await run_fix_loop(
        task=request.task,
        student_code=request.code,
        tests=request.tests,
        baseline=baseline,
        llm=llm,
        model=settings.fixer_model,
        runner=runner,
        max_attempts=request.max_fix_attempts or settings.max_fix_attempts,
        on_attempt=save_fix,
    )

    if not fix_outcome.success or fix_outcome.final is None:
        attempts = len(fix_outcome.attempts)
        internals["fix_attempts"] = [a.as_dict() for a in fix_outcome.attempts]
        await db.finish_failure(
            session_id,
            ErrorCode.fix_failed,
            f"Could not produce a working solution in {attempts} attempt(s)",
            internals,
        )
        return

    fixed = fix_outcome.final
    diff = codediff.unified(request.code, fixed.fixed_code)
    internals |= {
        "fixed_code": fixed.fixed_code,
        "fix_summary": fixed.summary,
        "diff": diff,
        "fix_attempts_used": fixed.number,
    }

    # --- loop 2: hint ----------------------------------------------------- #
    await db.set_stage(session_id, SessionStatus.running, SessionStage.hinting)

    async def save_hint(attempt: HintAttempt) -> None:
        await db.add_attempt(session_id, "hint", attempt.number, attempt.as_dict())

    hint_outcome = await run_hint_loop(
        task=request.task,
        student_code=request.code,
        fixed_code=fixed.fixed_code,
        diff=diff,
        mistakes=fixed.mistakes,
        llm=llm,
        hint_model=settings.hint_model,
        validator_model=settings.validator_model,
        max_attempts=request.max_hint_attempts or settings.max_hint_attempts,
        on_attempt=save_hint,
    )

    if not hint_outcome.success:
        last = hint_outcome.final
        internals["rejected_hints"] = [a.as_dict() for a in hint_outcome.attempts]
        issues = "; ".join(last.issues) if last else "no details"
        await db.finish_failure(
            session_id,
            ErrorCode.hint_rejected,
            f"The validator rejected the hint in {len(hint_outcome.attempts)} attempt(s): {issues}",
            internals,
        )
        return

    internals["hint_attempts_used"] = len(hint_outcome.attempts)
    await db.finish_success(
        session_id,
        SessionResult(
            outcome=Outcome.hint_ready,
            hint=hint_outcome.hint,
            mistakes=[
                MistakeOut(title=m.title, detail=m.detail, line=m.line or None)
                for m in fixed.mistakes
            ],
            tests_total=baseline.total,
            tests_passed_before=baseline.passed_count,
        ).model_dump(),
        internals,
    )
