from conftest import FakeLLM, report, sample_tests

from problem_helper.fixer import run_fix_loop
from problem_helper.schemas import FixResult, Mistake

MISTAKE = Mistake(title="wrong sign", detail="minus instead of plus", line=2)


def fix(code: str) -> FixResult:
    return FixResult(mistakes=[MISTAKE], fixed_code=code, summary="fixed the sign")


def runner_returning(*results):
    queue = list(results)

    async def runner(code: str):
        return queue.pop(0) if len(queue) > 1 else queue[0]

    return runner


async def run(llm, runner, max_attempts=3, **kwargs):
    return await run_fix_loop(
        task="sum of two numbers",
        student_code="print(a - b)",
        tests=sample_tests(),
        baseline=report(passed=False),
        llm=llm,
        model="fixer",
        runner=runner,
        max_attempts=max_attempts,
        **kwargs,
    )


async def test_succeeds_on_first_attempt():
    llm = FakeLLM({FixResult: [fix("print(a + b)")]})

    outcome = await run(llm, runner_returning(report(passed=True)))

    assert outcome.success
    assert len(outcome.attempts) == 1
    assert outcome.final.fixed_code == "print(a + b)"
    assert outcome.final.mistakes == [MISTAKE]
    assert llm.calls[0].model == "fixer"


async def test_second_attempt_gets_previous_failure_in_context():
    llm = FakeLLM({FixResult: [fix("first edit"), fix("second edit")]})

    outcome = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert outcome.success
    assert len(outcome.attempts) == 2
    second_prompt = llm.users_for(FixResult)[1]
    assert "Your previous fix did not pass the tests" in second_prompt
    assert "first edit" in second_prompt


async def test_gives_up_after_max_attempts():
    llm = FakeLLM({FixResult: [fix("still broken")]})
    attempts_seen = []

    outcome = await run(
        llm,
        runner_returning(report(passed=False)),
        max_attempts=2,
        on_attempt=lambda a: _collect(attempts_seen, a),
    )

    assert not outcome.success
    assert len(outcome.attempts) == 2
    assert len(attempts_seen) == 2
    assert not outcome.final.passed


async def _collect(sink, attempt):
    sink.append(attempt)
