"""The subprocess backend: `python -I` in its own process session under rlimits.

This is the weaker of the two backends and it is not a real sandbox — the process shares
the host kernel, the host filesystem and the host network namespace. It stays in the tree
because the test suite has to run on a machine without a container runtime, and because it
is the honest baseline the container backend is measured against.

Nothing selects it implicitly: `SANDBOX_BACKEND` names a backend and an unreachable Docker
daemon raises instead of quietly landing here. See `container.py` for what the difference
actually buys.
"""

from __future__ import annotations

import asyncio
import os
import signal
import sys
import time
from pathlib import Path

from ..schemas import TestCase
from .report import TestOutcome, outcome_from

SOLUTION_FILE = "solution.py"


def _limits(memory_mb: int, cpu_sec: int):  # pragma: no cover - runs in the child process
    def apply() -> None:
        import resource

        mem = memory_mb * 1024 * 1024
        resource.setrlimit(resource.RLIMIT_AS, (mem, mem))
        resource.setrlimit(resource.RLIMIT_CPU, (cpu_sec, cpu_sec + 1))
        resource.setrlimit(resource.RLIMIT_FSIZE, (1024 * 1024, 1024 * 1024))
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))

    return apply


async def run_one(
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
        SOLUTION_FILE,
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

    return outcome_from(
        index=index,
        test_input=test.input,
        expected_output=test.expected_output,
        stdout_raw=stdout_raw,
        stderr_raw=stderr_raw,
        exit_code=proc.returncode,
        timed_out=timed_out,
        duration_ms=int((time.monotonic() - started) * 1000),
        max_output_bytes=max_output_bytes,
    )


def _kill(proc: asyncio.subprocess.Process) -> None:
    try:
        if sys.platform != "win32":
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        else:  # pragma: no cover - windows
            proc.kill()
    except (ProcessLookupError, PermissionError):  # pragma: no cover
        pass
