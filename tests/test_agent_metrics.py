"""The Part 1 metrics. Pure functions, so these are the cheapest tests in the suite.

The cases worth reading are the ones about *acceptable alternatives*: a metric that only
recognises one correct trajectory is the failure mode this design exists to avoid, and it
would pass every test that used a single expected path.
"""

import pytest

from evals.agent_metrics import (
    Expectation,
    check_parameters,
    flaky,
    goal_completion,
    pass_rates,
    summarise,
    tool_parameter_accuracy,
    tool_selection_accuracy,
    trajectory_scores,
)
from evals.trajectory import ToolCall

SEARCH = ToolCall("search_corpus", {"query": "why does my binary search hang", "k": 5})
READ = ToolCall("get_learning_material", {"material_id": "algo-binary-search"})
TOPICS = ToolCall("list_material_topics", {})


def expectation(**overrides) -> Expectation:
    base = {
        "alternatives": [["search_corpus"], ["search_corpus", "get_learning_material"]],
        "parameters": {
            "search_corpus": {"mentions": [["binary search", "bisect"]], "k_between": [1, 10]}
        },
        "outcome": "hint_ready",
        "hint_must_mention": [["invariant", "boundary"]],
        "materials": ["algo-binary-search"],
    }
    return Expectation(**{**base, **overrides})


# --------------------------------------------------------------------------- #
# Tool selection
# --------------------------------------------------------------------------- #


@pytest.mark.parametrize(
    ("calls", "score"),
    [
        ([SEARCH], 1.0),
        ([SEARCH, READ], 1.0),
        ([READ, SEARCH], 1.0),  # order is not what this metric is about
        ([], 0.0),
        ([TOPICS], 0.0),
        ([SEARCH, SEARCH], 0.0),  # twice is a different plan, not the same one
    ],
)
def test_tool_selection_against_alternatives(calls, score):
    assert tool_selection_accuracy(calls, expectation()) == score


def test_calling_nothing_is_correct_when_the_case_allows_it():
    allows_silence = expectation(alternatives=[[], ["search_corpus"]])

    assert tool_selection_accuracy([], allows_silence) == 1.0
    assert tool_selection_accuracy([SEARCH], allows_silence) == 1.0


# --------------------------------------------------------------------------- #
# Parameters
# --------------------------------------------------------------------------- #


def test_parameters_are_judged_on_usability_not_wording():
    paraphrase = ToolCall("search_corpus", {"query": "my bisect loop never terminates", "k": 3})

    score, failures = tool_parameter_accuracy([paraphrase], expectation())

    assert score == 1.0
    assert failures == []


def test_an_off_topic_query_fails():
    off_topic = ToolCall("search_corpus", {"query": "how do I sort a list", "k": 3})

    score, failures = tool_parameter_accuracy([off_topic], expectation())

    assert score == 0.0
    assert "binary search" in failures[0]


def test_a_run_with_no_constrained_call_is_skipped_not_zeroed():
    score, failures = tool_parameter_accuracy([], expectation())

    assert score is None
    assert failures == []


def test_k_outside_the_tool_bounds_fails():
    ok, why = check_parameters(
        ToolCall("search_corpus", {"query": "binary search", "k": 99}), {"k_between": [1, 10]}
    )

    assert not ok
    assert "99" in why


def test_a_keyword_query_fails_min_words():
    ok, why = check_parameters(ToolCall("search_corpus", {"query": "bisect"}), {"min_words": 3})

    assert not ok
    assert "1 word" in why


def test_one_of_constrains_an_identifier_argument():
    ok, _ = check_parameters(READ, {"one_of": {"material_id": ["algo-binary-search"]}})
    bad, why = check_parameters(
        ToolCall("get_learning_material", {"material_id": "../../etc/passwd"}),
        {"one_of": {"material_id": ["algo-binary-search"]}},
    )

    assert ok
    assert not bad
    assert "etc/passwd" in why


# --------------------------------------------------------------------------- #
# Trajectory precision and recall
# --------------------------------------------------------------------------- #


def test_an_exact_trajectory_scores_one_on_both():
    precision, recall, matched = trajectory_scores([SEARCH, READ], expectation())

    assert (precision, recall) == (1.0, 1.0)
    assert matched == ["search_corpus", "get_learning_material"]


def test_an_extra_call_costs_precision_only():
    precision, recall, _ = trajectory_scores([SEARCH, READ, TOPICS], expectation())

    assert precision == pytest.approx(2 / 3)
    assert recall == 1.0


def test_a_missing_call_costs_recall_only():
    precision, recall, matched = trajectory_scores([SEARCH], expectation())

    assert (precision, recall) == (1.0, 1.0)
    assert matched == ["search_corpus"]  # scored against the alternative it matched


def test_the_wrong_order_costs_recall_without_zeroing_it():
    single = expectation(alternatives=[["search_corpus", "get_learning_material"]])

    precision, recall, _ = trajectory_scores([READ, SEARCH], single)

    assert 0 < recall < 1
    assert 0 < precision < 1


# --------------------------------------------------------------------------- #
# Goal completion
# --------------------------------------------------------------------------- #


def test_goal_completion_needs_the_outcome_and_the_concept():
    score, reasons = goal_completion(
        {"outcome": "hint_ready", "hint": "Your loop invariant is inconsistent."},
        expectation(),
        cited_materials=["algo-binary-search"],
    )

    assert score == 1.0
    assert reasons == []


def test_the_wrong_outcome_fails():
    score, reasons = goal_completion(
        {"outcome": "already_correct"}, expectation(), cited_materials=[]
    )

    assert score == 0.0
    assert "hint_ready" in reasons[0]


def test_a_hint_that_misses_the_concept_fails():
    score, reasons = goal_completion(
        {"outcome": "hint_ready", "hint": "Have another look at your code."},
        expectation(),
        cited_materials=[],
    )

    assert score == 0.0
    assert "invariant" in reasons[0]


def test_an_expected_error_code_is_a_completed_goal():
    refusal = Expectation(alternatives=[[]], outcome="", error_code="unsafe_input")

    score, reasons = goal_completion(
        {"error_code": "unsafe_input"}, refusal, cited_materials=[]
    )

    assert score == 1.0
    assert reasons == []


def test_citing_a_material_outside_the_expected_set_fails():
    score, reasons = goal_completion(
        {"outcome": "hint_ready", "hint": "Check the loop invariant."},
        expectation(),
        cited_materials=["algo-binary-search", "algo-heaps"],
    )

    assert score == 0.0
    assert "algo-heaps" in reasons[0]


def test_a_term_matches_as_a_stem_at_a_word_boundary():
    """`boundar` has to reach "boundaries"; it must not match inside another word."""
    stem = Expectation(alternatives=[[]], hint_must_mention=[["boundar"]])

    def score(hint: str) -> float:
        return goal_completion({"outcome": "hint_ready", "hint": hint}, stem, cited_materials=[])[0]

    assert score("The two boundaries are inconsistent.") == 1.0
    assert score("Your boundary condition is off.") == 1.0
    assert score("Have another look at your code.") == 0.0
    assert score("The subboundary case is fine.") == 0.0


# --------------------------------------------------------------------------- #
# Aggregation
# --------------------------------------------------------------------------- #


def test_pass_rates_separate_ever_from_always():
    assert pass_rates([True, False, False]) == {"pass@1": 1 / 3, "pass@k": 1.0, "pass^k": 0.0}
    assert pass_rates([True, True, True]) == {"pass@1": 1.0, "pass@k": 1.0, "pass^k": 1.0}
    assert pass_rates([False] * 3) == {"pass@1": 0.0, "pass@k": 0.0, "pass^k": 0.0}


def test_flaky_is_neither_always_nor_never():
    assert flaky([True, False, True])
    assert not flaky([True, True, True])
    assert not flaky([False, False, False])


def test_summarise_counts_the_skipped_runs_rather_than_averaging_zeros():
    rows = [{"tool_parameter_accuracy": 1.0}, {"tool_parameter_accuracy": None}]

    out = summarise(rows, ("tool_parameter_accuracy",))

    assert out["tool_parameter_accuracy"] == 1.0
    assert out["tool_parameter_accuracy_skipped"] == 1
