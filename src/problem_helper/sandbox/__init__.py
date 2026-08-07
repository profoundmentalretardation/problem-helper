"""Runs untrusted student/model code against stdin → stdout tests.

`run_tests` is the single swap point for where execution happens. It takes the same
arguments whatever the backend is, and it returns the same `TestReport`, so the graph, the
prompts and the database never learn which one ran:

- `local` — `python -I` in its own process session under rlimits. Contains resource use and
  nothing else: same kernel, same filesystem, same network as the service. It exists so the
  test suite runs on a machine without a container runtime.
- `docker` — a throwaway container per test with no network, a read-only rootfs, every
  capability dropped and a non-root user. This is the default.

There is deliberately no `auto`. A host where the daemon is down would then silently start
running untrusted code under the weak backend, and the failure mode of that mistake is
invisible: everything keeps working. `ensure_ready` raises `SandboxUnavailable` instead,
once, before the first test, and the orchestrator finishes the session as
`sandbox_unavailable`.

Static screening happens *before* any of this — see `problem_helper.codeshield`. The
container is what makes the screen safe to be imperfect; the screen is what keeps obviously
hostile code from being run at all.
"""

from __future__ import annotations

import logging
import tempfile
from pathlib import Path

import mlflow
from mlflow.entities import SpanType

from ..schemas import SandboxBackend, TestCase
from . import container, local
from .container import SandboxUnavailable
from .report import (
    TestOutcome,
    TestReport,
    normalize_output,
    outcome_from,
    refused,
    report_from_dict,
    truncate,
)

__all__ = [
    "DEFAULT_IMAGE",
    "SandboxBackend",
    "SandboxUnavailable",
    "TestOutcome",
    "TestReport",
    "normalize_output",
    "outcome_from",
    "refused",
    "report_from_dict",
    "run_tests",
    "truncate",
]

logger = logging.getLogger(__name__)

DEFAULT_IMAGE = "python:3.13-alpine"
SOLUTION_FILE = "solution.py"


@mlflow.trace(name="sandbox.run_tests", span_type=SpanType.TASK)
async def run_tests(
    code: str,
    tests: list[TestCase],
    *,
    backend: SandboxBackend | str = SandboxBackend.docker,
    image: str = DEFAULT_IMAGE,
    timeout_sec: float = 5.0,
    memory_mb: int = 256,
    max_output_bytes: int = 8_000,
) -> TestReport:
    """Runs `code` against every test sequentially and returns the report.

    Traced because a container start per test is a real part of a session's latency and
    autolog cannot see it — without this span the gap between two LLM calls is unexplained.
    """
    chosen = SandboxBackend(backend)
    if chosen is SandboxBackend.docker:
        await container.ensure_ready(image)

    report = TestReport()
    with tempfile.TemporaryDirectory(prefix="ph-sandbox-") as tmp:
        workdir = Path(tmp)
        solution = workdir / SOLUTION_FILE
        solution.write_text(code, encoding="utf-8")
        # The container runs as `nobody`, which has to be able to traverse the mount and
        # read the file; TemporaryDirectory creates it 0700.
        workdir.chmod(0o755)
        solution.chmod(0o644)

        for index, test in enumerate(tests):
            if chosen is SandboxBackend.docker:
                outcome = await container.run_one(
                    workdir,
                    index,
                    test,
                    timeout_sec=timeout_sec,
                    memory_mb=memory_mb,
                    max_output_bytes=max_output_bytes,
                    image=image,
                )
            else:
                outcome = await local.run_one(
                    workdir,
                    index,
                    test,
                    timeout_sec=timeout_sec,
                    memory_mb=memory_mb,
                    max_output_bytes=max_output_bytes,
                )
            report.outcomes.append(outcome)
    return report
