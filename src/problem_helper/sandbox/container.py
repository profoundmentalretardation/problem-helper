"""The Docker backend: one throwaway container per test, with nothing in it to abuse.

The subprocess backend contains *resource* use — memory, CPU, wall clock — and nothing
else. Code that runs there still sees the host filesystem and the host network. This
backend closes both, and it does it with runtime flags rather than with a language-level
denylist, which is the point: the code physically cannot open a socket, so no amount of
cleverness inside the interpreter gets one.

What each flag is for:

| flag | what it removes |
|---|---|
| `--network none` | no interface but loopback — exfiltration has nowhere to go |
| `--read-only` + `-v …:ro` | the solution cannot write anywhere except the tmpfs |
| `--tmpfs /tmp:…,noexec` | scratch space that cannot be turned into a payload |
| `--cap-drop ALL` | no capabilities at all, not even the default docker set |
| `--security-opt no-new-privileges` | setuid binaries in the image cannot raise privilege |
| `--user 65534:65534` | runs as `nobody`, never as root |
| `--pids-limit` | fork bombs hit a wall instead of the host scheduler |
| `--memory` / `--memory-swap` equal | a hard ceiling with swap disabled |
| `--cpus` | one core, so a busy loop cannot starve the service |
| `--ulimit fsize`/`nofile` | file-size and descriptor ceilings inside the namespace |

A container is started per test rather than per session on purpose: one test cannot leave
state behind for the next, and the per-test timeout stays a wall-clock kill of a single
process tree, exactly as in the subprocess backend.

The daemon is never probed lazily in the middle of a run. `ensure_ready` is called once,
before the first test, and raises `SandboxUnavailable` — the service reports the session as
failed rather than silently falling back to weaker isolation.
"""

from __future__ import annotations

import asyncio
import logging
import shutil
import time
from pathlib import Path
from uuid import uuid4

from ..schemas import TestCase
from .report import TestOutcome, outcome_from

logger = logging.getLogger(__name__)

SOLUTION_FILE = "solution.py"
MOUNT_POINT = "/sandbox"
PIDS_LIMIT = 64
NOFILE_LIMIT = 64
FSIZE_LIMIT = 1024 * 1024
TMPFS_MB = 16
KILL_GRACE_SEC = 5.0


class SandboxUnavailable(RuntimeError):
    """The configured backend cannot run code right now."""


_checked: set[str] = set()


async def ensure_ready(image: str) -> None:
    """Verifies the daemon answers and the image is present. Cached per image.

    Both failures are configuration errors with different fixes, so they get different
    messages: a dead daemon is an ops problem, a missing image is one `docker pull`.
    """
    if image in _checked:
        return
    if shutil.which("docker") is None:
        raise SandboxUnavailable(
            "SANDBOX_BACKEND=docker but the `docker` client is not on PATH. "
            "Install it, or set SANDBOX_BACKEND=local to accept the weaker isolation."
        )
    code, _, err = await _docker("version", "--format", "{{.Server.Version}}")
    if code != 0:
        raise SandboxUnavailable(
            f"the Docker daemon does not answer: {err.strip() or 'unknown error'}"
        )
    code, _, _ = await _docker("image", "inspect", image)
    if code != 0:
        raise SandboxUnavailable(
            f"sandbox image {image!r} is not present locally — run `docker pull {image}`"
        )
    _checked.add(image)
    logger.info("sandbox: docker backend ready, image %s", image)


def forget_readiness() -> None:
    """Drops the readiness cache; the tests use it to re-probe."""
    _checked.clear()


async def run_one(
    workdir: Path,
    index: int,
    test: TestCase,
    *,
    timeout_sec: float,
    memory_mb: int,
    max_output_bytes: int,
    image: str,
) -> TestOutcome:
    name = f"ph-sandbox-{uuid4().hex[:12]}"
    started = time.monotonic()
    proc = await asyncio.create_subprocess_exec(
        "docker",
        *_run_args(name, workdir, timeout_sec=timeout_sec, memory_mb=memory_mb, image=image),
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
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
        await _force_remove(name)
        # `docker run` returns as soon as the container is gone; the grace period only
        # covers a daemon that is slow to reap it.
        try:
            await asyncio.wait_for(proc.wait(), timeout=KILL_GRACE_SEC)
        except TimeoutError:  # pragma: no cover - daemon wedged
            proc.kill()
            await proc.wait()

    return outcome_from(
        index=index,
        test_input=test.input,
        expected_output=test.expected_output,
        stdout_raw=stdout_raw,
        stderr_raw=_clean_stderr(stderr_raw),
        exit_code=proc.returncode,
        timed_out=timed_out,
        duration_ms=int((time.monotonic() - started) * 1000),
        max_output_bytes=max_output_bytes,
    )


def _run_args(
    name: str, workdir: Path, *, timeout_sec: float, memory_mb: int, image: str
) -> list[str]:
    """The full `docker run` command line, kept in one place so it can be asserted on."""
    return [
        "run",
        "--rm",
        "--interactive",
        "--name",
        name,
        "--network",
        "none",
        "--read-only",
        "--tmpfs",
        f"/tmp:rw,noexec,nosuid,size={TMPFS_MB}m",
        "--memory",
        f"{memory_mb}m",
        "--memory-swap",
        f"{memory_mb}m",
        "--cpus",
        "1.0",
        "--pids-limit",
        str(PIDS_LIMIT),
        "--ulimit",
        f"nofile={NOFILE_LIMIT}:{NOFILE_LIMIT}",
        "--ulimit",
        f"fsize={FSIZE_LIMIT}:{FSIZE_LIMIT}",
        "--ulimit",
        f"cpu={max(1, int(timeout_sec) + 1)}",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--user",
        "65534:65534",
        "--volume",
        f"{workdir}:{MOUNT_POINT}:ro",
        "--workdir",
        MOUNT_POINT,
        "--env",
        "HOME=/tmp",
        "--env",
        "PYTHONIOENCODING=utf-8",
        "--env",
        "PYTHONDONTWRITEBYTECODE=1",
        image,
        "python",
        "-I",
        f"{MOUNT_POINT}/{SOLUTION_FILE}",
    ]


async def _force_remove(name: str) -> None:
    code, _, err = await _docker("rm", "--force", name)
    if code != 0:  # pragma: no cover - the container usually exits on its own
        logger.warning("sandbox: could not remove container %s: %s", name, err.strip())


async def _docker(*args: str) -> tuple[int, str, str]:
    proc = await asyncio.create_subprocess_exec(
        "docker",
        *args,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    out, err = await proc.communicate()
    return proc.returncode or 0, out.decode(errors="replace"), err.decode(errors="replace")


_DOCKER_NOISE = (
    b"Unable to find image",
    b"docker: Error response from daemon",
)


def _clean_stderr(raw: bytes) -> bytes:
    """Drops the daemon's own chatter so the fixer only sees the interpreter's stderr.

    A line from `docker` itself in the prompt would send the fixer looking for a bug in
    code that never ran.
    """
    lines = [
        line
        for line in raw.splitlines(keepends=True)
        if not any(line.startswith(prefix) for prefix in _DOCKER_NOISE)
    ]
    return b"".join(lines)
