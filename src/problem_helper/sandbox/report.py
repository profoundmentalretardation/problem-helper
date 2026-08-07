"""What a sandbox run returns, independent of which backend produced it.

The two backends differ only in how a process is started and contained; the outcome of one
test and the report over all of them are the same shape either way, which is what lets the
graph, the database and the prompts stay unaware of where the code actually ran.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass(slots=True)
class TestOutcome:
    index: int
    passed: bool
    input: str
    expected_output: str
    actual_output: str
    stderr: str
    exit_code: int | None
    timed_out: bool
    duration_ms: int

    def as_dict(self) -> dict:
        return {
            "index": self.index,
            "passed": self.passed,
            "input": self.input,
            "expected_output": self.expected_output,
            "actual_output": self.actual_output,
            "stderr": self.stderr,
            "exit_code": self.exit_code,
            "timed_out": self.timed_out,
            "duration_ms": self.duration_ms,
        }


@dataclass(slots=True)
class TestReport:
    outcomes: list[TestOutcome] = field(default_factory=list)

    @property
    def total(self) -> int:
        return len(self.outcomes)

    @property
    def passed_count(self) -> int:
        return sum(1 for o in self.outcomes if o.passed)

    @property
    def all_passed(self) -> bool:
        return bool(self.outcomes) and all(o.passed for o in self.outcomes)

    @property
    def failures(self) -> list[TestOutcome]:
        return [o for o in self.outcomes if not o.passed]

    def as_dict(self) -> dict:
        return {
            "total": self.total,
            "passed": self.passed_count,
            "outcomes": [o.as_dict() for o in self.outcomes],
        }

    def for_prompt(self, max_failures: int = 3) -> str:
        """Compact description of the failures, to be embedded in a prompt."""
        if not self.outcomes:
            return "No tests were run."
        if self.all_passed:
            return f"All tests passed ({self.passed_count}/{self.total})."

        lines = [f"Passed {self.passed_count} of {self.total}. Failures:"]
        for outcome in self.failures[:max_failures]:
            reason = "timed out" if outcome.timed_out else f"exit code {outcome.exit_code}"
            lines.append(
                f"\n--- test #{outcome.index + 1} ({reason}) ---\n"
                f"stdin:\n{outcome.input or '<empty>'}\n"
                f"expected:\n{outcome.expected_output or '<empty>'}\n"
                f"actual:\n{outcome.actual_output or '<empty>'}"
            )
            if outcome.stderr:
                lines.append(f"stderr:\n{outcome.stderr}")
        hidden = len(self.failures) - max_failures
        if hidden > 0:
            lines.append(f"\n...and {hidden} more failing tests.")
        return "\n".join(lines)


def report_from_dict(payload: dict) -> TestReport:
    """Rebuilds a report out of `as_dict()` — the graph state keeps plain dicts."""
    return TestReport(outcomes=[TestOutcome(**item) for item in payload.get("outcomes", [])])


def normalize_output(text: str) -> str:
    """Right-strip every line and drop blank lines at both ends."""
    lines = [line.rstrip() for line in text.splitlines()]
    while lines and not lines[-1]:
        lines.pop()
    while lines and not lines[0]:
        lines.pop(0)
    return "\n".join(lines)


def truncate(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    return text[:limit] + f"\n...[truncated, {len(text)} bytes total]"


def refused(tests: list, reason: str) -> TestReport:
    """A report for code that was never executed, because a guardrail refused it.

    It is shaped like a failing run rather than like an error so the fix loop handles it
    with the path it already has: the attempt is recorded, the reason reaches the fixer as
    stderr, and the retry budget bounds how many times it can happen.
    """
    return TestReport(
        outcomes=[
            TestOutcome(
                index=index,
                passed=False,
                input=getattr(test, "input", ""),
                expected_output=getattr(test, "expected_output", ""),
                actual_output="",
                stderr=reason,
                exit_code=None,
                timed_out=False,
                duration_ms=0,
            )
            for index, test in enumerate(tests)
        ]
    )


def outcome_from(
    *,
    index: int,
    test_input: str,
    expected_output: str,
    stdout_raw: bytes,
    stderr_raw: bytes,
    exit_code: int | None,
    timed_out: bool,
    duration_ms: int,
    max_output_bytes: int,
) -> TestOutcome:
    """Decides pass/fail and truncates — the one place both backends agree on the verdict."""
    stdout = truncate(stdout_raw.decode(errors="replace"), max_output_bytes)
    stderr = truncate(stderr_raw.decode(errors="replace"), max_output_bytes)
    passed = (
        not timed_out
        and exit_code == 0
        and normalize_output(stdout) == normalize_output(expected_output)
    )
    return TestOutcome(
        index=index,
        passed=passed,
        input=test_input,
        expected_output=expected_output,
        actual_output=stdout,
        stderr=stderr,
        exit_code=exit_code,
        timed_out=timed_out,
        duration_ms=duration_ms,
    )
