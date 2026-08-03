import pytest
from conftest import FakeLLM

from problem_helper.config import Settings
from problem_helper.db import Database
from problem_helper.orchestrator import process_session
from problem_helper.schemas import (
    ErrorCode,
    FixResult,
    HintResult,
    Mistake,
    Outcome,
    SessionStatus,
    SolveRequest,
    TestCase,
    ValidationResult,
)

BROKEN = "a, b = map(int, input().split())\nprint(a - b)\n"
CORRECT = "a, b = map(int, input().split())\nprint(a + b)\n"

MISTAKE = Mistake(title="wrong operator", detail="subtraction instead of addition", line=2)


def request(code: str = BROKEN, **kwargs) -> SolveRequest:
    return SolveRequest(
        task="Read two numbers and print their sum",
        code=code,
        tests=[
            TestCase(input="3 4\n", expected_output="7"),
            TestCase(input="10 5\n", expected_output="15"),
        ],
        **kwargs,
    )


def settings() -> Settings:
    return Settings(llm_api_key="test", max_fix_attempts=2, max_hint_attempts=2)


@pytest.fixture
async def db(tmp_path):
    database = await Database(str(tmp_path / "test.db")).connect()
    yield database
    await database.close()


async def run(db, llm, req: SolveRequest | None = None) -> dict:
    req = req or request()
    await db.create_session("s1", req.model_dump())
    await process_session("s1", req, db=db, llm=llm, settings=settings())
    return await db.get_session("s1")


def good_llm(approved: bool = True) -> FakeLLM:
    return FakeLLM(
        {
            FixResult: [FixResult(mistakes=[MISTAKE], fixed_code=CORRECT, summary="sign")],
            HintResult: [HintResult(hint="compare the operator on line 2 with the statement")],
            ValidationResult: [
                ValidationResult(approved=approved, issues=[] if approved else ["too vague"])
            ],
        }
    )


async def test_correct_code_skips_both_loops(db):
    llm = good_llm()

    record = await run(db, llm, request(code=CORRECT))

    assert record["status"] == SessionStatus.succeeded
    assert record["result"]["outcome"] == Outcome.already_correct
    assert record["result"]["hint"] is None
    assert record["result"]["tests_passed_before"] == 2
    assert llm.calls == []


async def test_happy_path_produces_hint_and_diff(db):
    llm = good_llm()

    record = await run(db, llm)

    assert record["status"] == SessionStatus.succeeded
    result = record["result"]
    assert result["outcome"] == Outcome.hint_ready
    assert result["hint"].startswith("compare the operator")
    assert result["mistakes"][0]["line"] == 2
    assert result["tests_passed_before"] == 0

    internals = record["internals"]
    assert internals["fixed_code"] == CORRECT
    assert "-print(a - b)" in internals["diff"]
    assert internals["fix_attempts_used"] == 1

    attempts = await db.get_attempts("s1")
    assert [a["kind"] for a in attempts] == ["fix", "hint"]
    assert attempts[0]["payload"]["passed"] is True


async def test_fix_failed_when_model_never_repairs_code(db):
    llm = FakeLLM(
        {
            FixResult: [FixResult(mistakes=[MISTAKE], fixed_code=BROKEN, summary="nothing")],
            HintResult: [HintResult(hint="should never be reached")],
            ValidationResult: [ValidationResult(approved=True, issues=[])],
        }
    )

    record = await run(db, llm)

    assert record["status"] == SessionStatus.failed
    assert record["error_code"] == ErrorCode.fix_failed
    assert len(record["internals"]["fix_attempts"]) == 2
    assert record["result"] is None
    assert len(await db.get_attempts("s1")) == 2


async def test_hint_rejected_when_validator_never_approves(db):
    llm = good_llm(approved=False)

    record = await run(db, llm)

    assert record["status"] == SessionStatus.failed
    assert record["error_code"] == ErrorCode.hint_rejected
    assert "too vague" in record["error_message"]
    assert len(record["internals"]["rejected_hints"]) == 2
    assert record["internals"]["fixed_code"] == CORRECT


async def test_llm_failure_is_recorded_as_internal_error(db):
    class Boom:
        async def structured(self, **kwargs):
            raise RuntimeError("provider is down")

    record = await run(db, Boom())

    assert record["status"] == SessionStatus.failed
    assert record["error_code"] == ErrorCode.internal_error
    assert "provider is down" in record["error_message"]


async def test_request_overrides_attempt_limits(db):
    llm = FakeLLM(
        {
            FixResult: [FixResult(mistakes=[MISTAKE], fixed_code=BROKEN, summary="nothing")],
            HintResult: [HintResult(hint="x")],
            ValidationResult: [ValidationResult(approved=True, issues=[])],
        }
    )

    await run(db, llm, request(max_fix_attempts=1))

    assert len(await db.get_attempts("s1")) == 1
