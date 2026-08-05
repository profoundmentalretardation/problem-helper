"""The sample catalog has to be true, not just present.

Every sample is executed in the real sandbox: the reference solution must pass all of its
tests and the broken one must fail at least one. A sample whose "broken" code accidentally
passes would turn into a test of the already_correct path and would never produce the hint
it was written to demonstrate — and that is invisible by reading.
"""

import pytest

from problem_helper import materials, samples
from problem_helper.sandbox import run_tests

SAMPLES = samples.all()
IDS = [s.id for s in SAMPLES]


@pytest.mark.parametrize("sample", SAMPLES, ids=IDS)
async def test_the_reference_solution_passes_every_test(sample):
    report = await run_tests(sample.solution, sample.tests, timeout_sec=5.0)

    assert report.all_passed, _explain(sample.solution, report)


@pytest.mark.parametrize("sample", SAMPLES, ids=IDS)
async def test_the_broken_code_fails_at_least_one_test(sample):
    report = await run_tests(sample.code, sample.tests, timeout_sec=5.0)

    assert not report.all_passed, "this sample would take the already_correct path"


@pytest.mark.parametrize("sample", SAMPLES, ids=IDS)
def test_every_sample_points_at_a_real_material(sample):
    assert materials.get(sample.expected_material) is not None


def test_sample_ids_are_unique_and_lookup_works():
    assert len(set(IDS)) == len(IDS)
    assert samples.get(IDS[0]).title
    assert samples.get("nope") is None


def test_the_catalog_covers_several_topics():
    assert len(SAMPLES) >= 8
    assert len({s.topic for s in SAMPLES}) >= 5


def _explain(code: str, report) -> str:
    failing = [o for o in report.outcomes if not o.passed]
    first = failing[0]
    return (
        f"reference solution failed test #{first.index + 1}: "
        f"expected {first.expected_output!r}, got {first.actual_output!r}, "
        f"stderr {first.stderr[:200]!r}\n{code}"
    )
