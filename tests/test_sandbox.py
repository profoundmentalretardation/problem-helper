from problem_helper.sandbox import normalize_output, run_tests
from problem_helper.schemas import TestCase

SUM_OK = "a, b = map(int, input().split())\nprint(a + b)\n"
SUM_BROKEN = "a, b = map(int, input().split())\nprint(a - b)\n"


def cases() -> list[TestCase]:
    return [
        TestCase(input="3 4\n", expected_output="7"),
        TestCase(input="10 5\n", expected_output="15"),
    ]


async def test_all_tests_pass():
    report = await run_tests(SUM_OK, cases())

    assert report.all_passed
    assert report.passed_count == 2
    assert report.failures == []
    assert "All tests passed" in report.for_prompt()


async def test_wrong_answer_is_reported():
    report = await run_tests(SUM_BROKEN, cases())

    assert not report.all_passed
    assert report.passed_count == 0
    assert report.failures[0].actual_output.strip() == "-1"
    prompt = report.for_prompt()
    assert "test #1" in prompt
    assert "expected" in prompt


async def test_runtime_error_captured_in_stderr():
    report = await run_tests("raise ValueError('boom')\n", [TestCase(expected_output="")])

    outcome = report.outcomes[0]
    assert not outcome.passed
    assert outcome.exit_code == 1
    assert "ValueError" in outcome.stderr


async def test_syntax_error_does_not_crash_runner():
    report = await run_tests("def f(:\n", [TestCase(expected_output="")])

    assert not report.all_passed
    assert "SyntaxError" in report.outcomes[0].stderr


async def test_infinite_loop_is_killed_by_timeout():
    report = await run_tests(
        "while True:\n    pass\n",
        [TestCase(expected_output="")],
        timeout_sec=1.0,
    )

    outcome = report.outcomes[0]
    assert outcome.timed_out
    assert not outcome.passed
    assert outcome.duration_ms < 5_000


async def test_output_is_truncated():
    report = await run_tests(
        "print('x' * 10_000)\n",
        [TestCase(expected_output="")],
        max_output_bytes=100,
    )

    assert len(report.outcomes[0].actual_output) < 200
    assert "truncated" in report.outcomes[0].actual_output


async def test_trailing_whitespace_ignored_in_comparison():
    report = await run_tests("print('7  ')\n", [TestCase(expected_output="7\n\n")])

    assert report.all_passed


async def test_non_ascii_output_survives_the_pipe():
    report = await run_tests("print('чётность ✓')\n", [TestCase(expected_output="чётность ✓")])

    assert report.all_passed


def test_normalize_output():
    assert normalize_output("  7 \n\n") == "  7"
    assert normalize_output("a \nb\t\n") == "a\nb"
