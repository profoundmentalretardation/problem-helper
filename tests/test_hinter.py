from conftest import FakeLLM

from problem_helper.hinter import run_hint_loop
from problem_helper.schemas import HintResult, Mistake, ValidationResult

MISTAKES = [Mistake(title="wrong sign", detail="minus instead of plus", line=2)]


async def run(llm, max_attempts=3):
    return await run_hint_loop(
        task="sum of two numbers",
        student_code="print(a - b)",
        fixed_code="print(a + b)",
        diff="-print(a - b)\n+print(a + b)",
        mistakes=MISTAKES,
        llm=llm,
        hint_model="cheap",
        validator_model="strong",
        max_attempts=max_attempts,
    )


async def test_approved_on_first_try():
    llm = FakeLLM(
        {
            HintResult: [HintResult(hint="look at the operator on line 2")],
            ValidationResult: [ValidationResult(approved=True, issues=[])],
        }
    )

    outcome = await run(llm)

    assert outcome.success
    assert outcome.hint == "look at the operator on line 2"
    assert len(outcome.attempts) == 1
    assert [c.model for c in llm.calls] == ["cheap", "strong"]


async def test_validator_remarks_go_into_next_generation():
    llm = FakeLLM(
        {
            HintResult: [
                HintResult(hint="check your code"),
                HintResult(hint="compare the operator with the statement"),
            ],
            ValidationResult: [
                ValidationResult(approved=False, issues=["too vague"]),
                ValidationResult(approved=True, issues=[]),
            ],
        }
    )

    outcome = await run(llm)

    assert outcome.success
    assert outcome.hint == "compare the operator with the statement"
    second_prompt = llm.users_for(HintResult)[1]
    assert "Rejected hint #1" in second_prompt
    assert "too vague" in second_prompt


async def test_fails_when_never_approved():
    llm = FakeLLM(
        {
            HintResult: [HintResult(hint="check your code")],
            ValidationResult: [ValidationResult(approved=False, issues=["not specific"])],
        }
    )

    outcome = await run(llm, max_attempts=2)

    assert not outcome.success
    assert outcome.hint is None
    assert len(outcome.attempts) == 2
    assert outcome.final.issues == ["not specific"]
