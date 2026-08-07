"""API schemas and structured-output schemas for the agents."""

from enum import StrEnum
from typing import Literal

from pydantic import BaseModel, Field

# --------------------------------------------------------------------------- #
# Request
# --------------------------------------------------------------------------- #


class TestCase(BaseModel):
    """A single test: feed `input` to stdin, expect `expected_output` on stdout."""

    __test__ = False  # keep pytest from collecting this model as a test class

    input: str = ""
    expected_output: str


class SolveRequest(BaseModel):
    task: str = Field(min_length=1, description="Problem statement as plain text")
    code: str = Field(min_length=1, description="The student's broken code")
    tests: list[TestCase] = Field(min_length=1)
    language: Literal["python"] = "python"
    max_fix_attempts: int | None = Field(default=None, ge=1, le=10)
    max_hint_attempts: int | None = Field(default=None, ge=1, le=10)


# --------------------------------------------------------------------------- #
# Statuses
# --------------------------------------------------------------------------- #


class SessionStatus(StrEnum):
    pending = "pending"
    running = "running"
    succeeded = "succeeded"
    failed = "failed"


class SessionStage(StrEnum):
    queued = "queued"
    running_tests = "running_tests"
    fixing = "fixing"
    hinting = "hinting"
    done = "done"


class Outcome(StrEnum):
    already_correct = "already_correct"
    hint_ready = "hint_ready"


class ErrorCode(StrEnum):
    fix_failed = "fix_failed"
    hint_rejected = "hint_rejected"
    internal_error = "internal_error"


# --------------------------------------------------------------------------- #
# Responses
# --------------------------------------------------------------------------- #


class SessionCreated(BaseModel):
    session_id: str
    status: SessionStatus


class SampleView(BaseModel):
    """A ready-made session for the playground.

    Deliberately without `samples.Sample.solution`: the catalog carries the reference
    solution so the tests can prove the sample is well-formed, and handing it to a browser
    would defeat the entire point of a service that answers with hints.
    """

    id: str
    title: str
    topic: str
    task: str
    code: str
    tests: list[TestCase]


class MistakeOut(BaseModel):
    title: str
    detail: str
    line: int | None = None


class MaterialRef(BaseModel):
    """A study material the hint agent pulled through the tools."""

    id: str
    title: str
    topic: str
    summary: str


class SessionResult(BaseModel):
    """The part of the result that may be shown to the student."""

    outcome: Outcome
    hint: str | None = None
    mistakes: list[MistakeOut] = Field(default_factory=list)
    materials: list[MaterialRef] = Field(default_factory=list)
    tests_total: int
    tests_passed_before: int


class SessionError(BaseModel):
    code: ErrorCode
    message: str


class SessionView(BaseModel):
    session_id: str
    status: SessionStatus
    stage: SessionStage
    created_at: str
    updated_at: str
    result: SessionResult | None = None
    error: SessionError | None = None


class SessionDebugView(SessionView):
    """Full internal trace — for the teacher/debugging, never for the student."""

    request: SolveRequest | None = None
    internals: dict | None = None
    attempts: list[dict] = Field(default_factory=list)


# --------------------------------------------------------------------------- #
# Agent structured output
# --------------------------------------------------------------------------- #


class Mistake(BaseModel):
    title: str = Field(description="Short name of the mistake")
    detail: str = Field(description="What exactly is wrong and why it breaks the solution")
    line: int = Field(description="Line number in the student's code, 0 if not line-specific")


class FixResult(BaseModel):
    mistakes: list[Mistake]
    fixed_code: str = Field(description="The whole fixed file, no markdown wrapper")
    summary: str = Field(description="One or two sentences about the essence of the fix")


class HintResult(BaseModel):
    hint: str = Field(description="Hint for the student, without the solution code")
    related_material_ids: list[str] = Field(
        description=(
            "Ids of the study materials worth reading, taken from the tool results; "
            "empty list when no material was pulled or none of them fits"
        )
    )


class ValidationResult(BaseModel):
    approved: bool
    issues: list[str] = Field(description="Remarks about the hint; empty when approved=true")
