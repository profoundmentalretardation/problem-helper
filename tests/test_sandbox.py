"""Both backends, against the same assertions.

The point of parametrising rather than testing the container separately is that the two
backends are a swap point: if `docker` and `local` ever disagree about what "passed" means,
about how output is truncated or about what a timeout looks like, the graph above them is
reading a different report depending on the deployment. Every behavioural test therefore
runs twice.

The docker parameter skips itself when there is no daemon or no image, so the suite still
runs on a machine without a container runtime — which is the entire reason the `local`
backend is still in the tree.
"""

import shutil
import subprocess

import pytest

from problem_helper.sandbox import DEFAULT_IMAGE, container, normalize_output, run_tests
from problem_helper.schemas import SandboxBackend, TestCase

SUM_OK = "a, b = map(int, input().split())\nprint(a + b)\n"
SUM_BROKEN = "a, b = map(int, input().split())\nprint(a - b)\n"


def _docker_ready() -> bool:
    if shutil.which("docker") is None:
        return False
    probe = subprocess.run(
        ["docker", "image", "inspect", DEFAULT_IMAGE], capture_output=True, check=False
    )
    return probe.returncode == 0


DOCKER_READY = _docker_ready()

BACKENDS = [
    pytest.param(SandboxBackend.local, id="local"),
    pytest.param(
        SandboxBackend.docker,
        id="docker",
        marks=pytest.mark.skipif(
            not DOCKER_READY, reason=f"no docker daemon or no {DEFAULT_IMAGE} image"
        ),
    ),
]


@pytest.fixture(params=BACKENDS)
def backend(request):
    return request.param


def cases() -> list[TestCase]:
    return [
        TestCase(input="3 4\n", expected_output="7"),
        TestCase(input="10 5\n", expected_output="15"),
    ]


async def test_all_tests_pass(backend):
    report = await run_tests(SUM_OK, cases(), backend=backend)

    assert report.all_passed
    assert report.passed_count == 2
    assert report.failures == []
    assert "All tests passed" in report.for_prompt()


async def test_wrong_answer_is_reported(backend):
    report = await run_tests(SUM_BROKEN, cases(), backend=backend)

    assert not report.all_passed
    assert report.passed_count == 0
    assert report.failures[0].actual_output.strip() == "-1"
    prompt = report.for_prompt()
    assert "test #1" in prompt
    assert "expected" in prompt


async def test_runtime_error_captured_in_stderr(backend):
    report = await run_tests(
        "raise ValueError('boom')\n", [TestCase(expected_output="")], backend=backend
    )

    outcome = report.outcomes[0]
    assert not outcome.passed
    assert outcome.exit_code == 1
    assert "ValueError" in outcome.stderr


async def test_syntax_error_does_not_crash_runner(backend):
    report = await run_tests("def f(:\n", [TestCase(expected_output="")], backend=backend)

    assert not report.all_passed
    assert "SyntaxError" in report.outcomes[0].stderr


async def test_infinite_loop_is_killed_by_timeout(backend):
    report = await run_tests(
        "while True:\n    pass\n",
        [TestCase(expected_output="")],
        backend=backend,
        timeout_sec=1.0,
    )

    outcome = report.outcomes[0]
    assert outcome.timed_out
    assert not outcome.passed
    assert outcome.duration_ms < 10_000


async def test_output_is_truncated(backend):
    report = await run_tests(
        "print('x' * 10_000)\n",
        [TestCase(expected_output="")],
        backend=backend,
        max_output_bytes=100,
    )

    assert len(report.outcomes[0].actual_output) < 200
    assert "truncated" in report.outcomes[0].actual_output


async def test_trailing_whitespace_ignored_in_comparison(backend):
    report = await run_tests(
        "print('7  ')\n", [TestCase(expected_output="7\n\n")], backend=backend
    )

    assert report.all_passed


async def test_non_ascii_output_survives_the_pipe(backend):
    report = await run_tests(
        "print('чётность ✓')\n", [TestCase(expected_output="чётность ✓")], backend=backend
    )

    assert report.all_passed


def test_normalize_output():
    assert normalize_output("  7 \n\n") == "  7"
    assert normalize_output("a \nb\t\n") == "a\nb"


# --------------------------------------------------------------------------- #
# What the container backend buys, and what it does when it cannot deliver it
# --------------------------------------------------------------------------- #


@pytest.mark.skipif(not DOCKER_READY, reason="needs a docker daemon")
async def test_container_has_no_network():
    """The one property the subprocess backend cannot offer at all."""
    probe = (
        "import socket\n"
        "try:\n"
        "    socket.create_connection(('1.1.1.1', 53), timeout=2)\n"
        "    print('REACHED')\n"
        "except OSError as exc:\n"
        "    print('BLOCKED')\n"
    )
    report = await run_tests(
        probe,
        [TestCase(expected_output="BLOCKED")],
        backend=SandboxBackend.docker,
        timeout_sec=10.0,
    )

    assert report.all_passed, report.outcomes[0].actual_output


@pytest.mark.skipif(not DOCKER_READY, reason="needs a docker daemon")
async def test_container_filesystem_is_read_only():
    probe = (
        "try:\n"
        "    open('/sandbox/escape.txt', 'w').write('x')\n"
        "    print('WROTE')\n"
        "except OSError:\n"
        "    print('READONLY')\n"
    )
    report = await run_tests(
        probe,
        [TestCase(expected_output="READONLY")],
        backend=SandboxBackend.docker,
        timeout_sec=10.0,
    )

    assert report.all_passed, report.outcomes[0].actual_output


@pytest.mark.skipif(not DOCKER_READY, reason="needs a docker daemon")
async def test_container_does_not_run_as_root():
    report = await run_tests(
        "import os\nprint(os.getuid())\n",
        [TestCase(expected_output="65534")],
        backend=SandboxBackend.docker,
        timeout_sec=10.0,
    )

    assert report.all_passed, report.outcomes[0].actual_output


async def test_missing_image_is_refused_rather_than_downgraded():
    """The whole point of having no `auto`: a misconfiguration is loud."""
    container.forget_readiness()
    try:
        with pytest.raises(container.SandboxUnavailable) as excinfo:
            await run_tests(
                SUM_OK,
                cases(),
                backend=SandboxBackend.docker,
                image="problem-helper/definitely-not-pulled:0",
            )
    finally:
        container.forget_readiness()

    assert "docker pull" in str(excinfo.value)


def test_run_args_carry_every_isolation_flag():
    """A flag dropped by an editing accident is a silent loss of isolation."""
    args = container._run_args(
        "ph-sandbox-test", "/tmp/work", timeout_sec=5.0, memory_mb=256, image=DEFAULT_IMAGE
    )
    joined = " ".join(args)

    assert "--network none" in joined
    assert "--read-only" in args
    assert "--cap-drop ALL" in joined
    assert "--security-opt no-new-privileges" in joined
    assert "--user 65534:65534" in joined
    assert "--pids-limit" in args
    assert f"/tmp/work:{container.MOUNT_POINT}:ro" in args
