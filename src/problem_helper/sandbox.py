"""Runs untrusted student/model code against stdin → stdout tests.

MVP isolation: a separate `python -I` process in its own process session, a temporary cwd,
CPU/memory/file-size limits, hard kill on timeout, truncation of captured output.
This is not a real sandbox (no namespaces, no network ban) — in production a container
takes this place while `run_tests` keeps the same interface.
"""

from __future__ import annotations

import asyncio
import os
import signal
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

from .schemas import TestCase

_SOLUTION_FILE = "solution.py"


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


def normalize_output(text: str) -> str:
    """Right-strip every line and drop blank lines at both ends."""
    lines = [line.rstrip() for line in text.splitlines()]
    while lines and not lines[-1]:
        lines.pop()
    while lines and not lines[0]:
        lines.pop(0)
    return "\n".join(lines)


def _truncate(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    return text[:limit] + f"\n...[truncated, {len(text)} bytes total]"


def _limits(memory_mb: int, cpu_sec: int):  # pragma: no cover - runs in the child process
    def apply() -> None:
        import resource

        mem = memory_mb * 1024 * 1024
        resource.setrlimit(resource.RLIMIT_AS, (mem, mem))
        resource.setrlimit(resource.RLIMIT_CPU, (cpu_sec, cpu_sec + 1))
        resource.setrlimit(resource.RLIMIT_FSIZE, (1024 * 1024, 1024 * 1024))
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))

    return apply


async def _run_one(
    workdir: Path,
    index: int,
    test: TestCase,
    *,
    timeout_sec: float,
    memory_mb: int,
    max_output_bytes: int,
) -> TestOutcome:
    started = time.monotonic()
    popen_kwargs: dict = {}
    if sys.platform != "win32":
        popen_kwargs["preexec_fn"] = _limits(memory_mb, max(1, int(timeout_sec) + 1))
        popen_kwargs["start_new_session"] = True

    proc = await asyncio.create_subprocess_exec(
        sys.executable,
        "-I",
        _SOLUTION_FILE,
        cwd=workdir,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        env={"PATH": "/usr/bin:/bin", "HOME": str(workdir), "PYTHONIOENCODING": "utf-8"},
        **popen_kwargs,
    )

    timed_out = False
    stdout_raw = b""
    stderr_raw = b""
    try:
        stdout_raw, stderr_raw = await asyncio.wait_for(
            proc.communicate(input=test.input.encode()), timeout=timeout_sec
        )
    except TimeoutError:
        timed_out = True
        _kill(proc)
        await proc.wait()

    duration_ms = int((time.monotonic() - started) * 1000)
    stdout = _truncate(stdout_raw.decode(errors="replace"), max_output_bytes)
    stderr = _truncate(stderr_raw.decode(errors="replace"), max_output_bytes)
    passed = (
        not timed_out
        and proc.returncode == 0
        and normalize_output(stdout) == normalize_output(test.expected_output)
    )

    return TestOutcome(
        index=index,
        passed=passed,
        input=test.input,
        expected_output=test.expected_output,
        actual_output=stdout,
        stderr=stderr,
        exit_code=proc.returncode,
        timed_out=timed_out,
        duration_ms=duration_ms,
    )


def _kill(proc: asyncio.subprocess.Process) -> None:
    try:
        if sys.platform != "win32":
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        else:  # pragma: no cover - windows
            proc.kill()
    except (ProcessLookupError, PermissionError):  # pragma: no cover
        pass


async def run_tests(
    code: str,
    tests: list[TestCase],
    *,
    timeout_sec: float = 5.0,
    memory_mb: int = 256,
    max_output_bytes: int = 8_000,
) -> TestReport:
    """Runs `code` against every test sequentially and returns the report."""
    report = TestReport()
    with tempfile.TemporaryDirectory(prefix="ph-sandbox-") as tmp:
        workdir = Path(tmp)
        (workdir / _SOLUTION_FILE).write_text(code, encoding="utf-8")
        for index, test in enumerate(tests):
            report.outcomes.append(
                await _run_one(
                    workdir,
                    index,
                    test,
                    timeout_sec=timeout_sec,
                    memory_mb=memory_mb,
                    max_output_bytes=max_output_bytes,
                )
            )
    return report
