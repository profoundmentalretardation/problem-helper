"""Loop 1: the strong model repairs the student's code until the tests go green."""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

from . import prompts
from .llm import LLMProtocol
from .sandbox import TestReport
from .schemas import FixResult, Mistake, TestCase

logger = logging.getLogger(__name__)

Runner = Callable[[str], Awaitable[TestReport]]
"""Runs arbitrary code against this session's tests."""


@dataclass(slots=True)
class FixAttempt:
    number: int
    fixed_code: str
    mistakes: list[Mistake]
    summary: str
    report: TestReport

    @property
    def passed(self) -> bool:
        return self.report.all_passed

    def as_dict(self) -> dict:
        return {
            "attempt": self.number,
            "passed": self.passed,
            "summary": self.summary,
            "fixed_code": self.fixed_code,
            "mistakes": [m.model_dump() for m in self.mistakes],
            "tests": self.report.as_dict(),
        }


@dataclass(slots=True)
class FixOutcome:
    success: bool
    attempts: list[FixAttempt] = field(default_factory=list)

    @property
    def final(self) -> FixAttempt | None:
        return self.attempts[-1] if self.attempts else None


async def run_fix_loop(
    *,
    task: str,
    student_code: str,
    tests: list[TestCase],
    baseline: TestReport,
    llm: LLMProtocol,
    model: str,
    runner: Runner,
    max_attempts: int,
    on_attempt: Callable[[FixAttempt], Awaitable[None]] | None = None,
) -> FixOutcome:
    outcome = FixOutcome(success=False)
    previous_code: str | None = None
    previous_report: TestReport | None = None

    for number in range(1, max_attempts + 1):
        user = prompts.fixer_user(
            task=task,
            student_code=student_code,
            tests=tests,
            baseline=baseline,
            previous_code=previous_code,
            previous_report=previous_report,
        )
        result: FixResult = await llm.structured(
            model=model, system=prompts.FIXER_SYSTEM, user=user, schema=FixResult
        )
        report = await runner(result.fixed_code)
        attempt = FixAttempt(
            number=number,
            fixed_code=result.fixed_code,
            mistakes=result.mistakes,
            summary=result.summary,
            report=report,
        )
        outcome.attempts.append(attempt)
        if on_attempt is not None:
            await on_attempt(attempt)

        logger.info(
            "fix attempt %s/%s: %s/%s tests passed",
            number,
            max_attempts,
            report.passed_count,
            report.total,
        )
        if attempt.passed:
            outcome.success = True
            return outcome

        previous_code = result.fixed_code
        previous_report = report

    return outcome
