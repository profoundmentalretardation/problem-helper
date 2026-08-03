"""Loop 2: a cheap model writes the hint, a stronger model validates it.

The conversation history is not carried over: every iteration builds a fresh context with
the rejected hints and the validator's remarks stitched in — that keeps the token bill low.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

from . import prompts
from .llm import LLMProtocol
from .schemas import HintResult, Mistake, ValidationResult

logger = logging.getLogger(__name__)


@dataclass(slots=True)
class HintAttempt:
    number: int
    hint: str
    approved: bool
    issues: list[str]

    def as_dict(self) -> dict:
        return {
            "attempt": self.number,
            "hint": self.hint,
            "approved": self.approved,
            "issues": self.issues,
        }


@dataclass(slots=True)
class HintOutcome:
    success: bool
    attempts: list[HintAttempt] = field(default_factory=list)

    @property
    def final(self) -> HintAttempt | None:
        return self.attempts[-1] if self.attempts else None

    @property
    def hint(self) -> str | None:
        return self.attempts[-1].hint if self.success and self.attempts else None


async def run_hint_loop(
    *,
    task: str,
    student_code: str,
    fixed_code: str,
    diff: str,
    mistakes: list[Mistake],
    llm: LLMProtocol,
    hint_model: str,
    validator_model: str,
    max_attempts: int,
    on_attempt: Callable[[HintAttempt], Awaitable[None]] | None = None,
) -> HintOutcome:
    outcome = HintOutcome(success=False)
    rejected: list[tuple[str, list[str]]] = []

    for number in range(1, max_attempts + 1):
        generated: HintResult = await llm.structured(
            model=hint_model,
            system=prompts.HINT_SYSTEM,
            user=prompts.hint_user(
                task=task,
                student_code=student_code,
                fixed_code=fixed_code,
                diff=diff,
                mistakes=mistakes,
                rejected=rejected,
            ),
            schema=HintResult,
        )
        verdict: ValidationResult = await llm.structured(
            model=validator_model,
            system=prompts.VALIDATOR_SYSTEM,
            user=prompts.validator_user(
                task=task,
                student_code=student_code,
                fixed_code=fixed_code,
                diff=diff,
                hint=generated.hint,
            ),
            schema=ValidationResult,
        )

        attempt = HintAttempt(
            number=number,
            hint=generated.hint,
            approved=verdict.approved,
            issues=verdict.issues,
        )
        outcome.attempts.append(attempt)
        if on_attempt is not None:
            await on_attempt(attempt)

        logger.info(
            "hint attempt %s/%s: approved=%s, %s remark(s)",
            number,
            max_attempts,
            verdict.approved,
            len(verdict.issues),
        )
        if verdict.approved:
            outcome.success = True
            return outcome

        rejected.append((generated.hint, verdict.issues))

    return outcome
