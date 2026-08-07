"""Tests for the retrieval metrics and the eval set itself.

A scorer nobody tested is a number nobody should trust, so the five metrics are checked
against hand-computed values, against scikit-learn where an independent implementation
exists, and on the edge cases the harness actually hits: an empty golden set, a ranking with
no hit, and a golden set larger than k.
"""

import math

import pytest
from sklearn.metrics import ndcg_score

from evals.dataset import load, resolve_all, resolve_golden
from evals.retrieval_metrics import (
    aggregate,
    hit_rate_at_k,
    ndcg_at_k,
    precision_at_k,
    recall_at_k,
    reciprocal_rank,
    score_case,
)
from problem_helper.retrieval import build_chunks

RANKING = ["a", "b", "c", "d", "e"]
GOLDEN = {"b", "e", "z"}  # "z" is relevant but never retrieved


# --------------------------------------------------------------------------- #
# The five metrics
# --------------------------------------------------------------------------- #


def test_hit_rate_is_binary():
    assert hit_rate_at_k(RANKING, GOLDEN, 5) == 1.0
    assert hit_rate_at_k(RANKING, GOLDEN, 1) == 0.0  # "a" is not relevant


def test_precision_divides_by_k_not_by_what_was_returned():
    assert precision_at_k(RANKING, GOLDEN, 5) == pytest.approx(2 / 5)
    # only two chunks returned, one relevant, k=5 → 1/5, not 1/2
    assert precision_at_k(["a", "b"], GOLDEN, 5) == pytest.approx(1 / 5)


def test_recall_divides_by_the_whole_golden_set():
    assert recall_at_k(RANKING, GOLDEN, 5) == pytest.approx(2 / 3)
    assert recall_at_k(RANKING, GOLDEN, 2) == pytest.approx(1 / 3)


def test_reciprocal_rank_reads_the_first_hit_only():
    assert reciprocal_rank(RANKING, GOLDEN) == pytest.approx(1 / 2)
    assert reciprocal_rank(["x", "y", "b"], GOLDEN) == pytest.approx(1 / 3)
    assert reciprocal_rank(["x", "y"], GOLDEN) == 0.0


def test_reciprocal_rank_respects_k():
    assert reciprocal_rank(RANKING, GOLDEN, k=1) == 0.0


def test_ndcg_matches_the_hand_computation():
    # hits at ranks 2 and 5; ideal ranking puts all three (capped at k=5) first
    dcg = 1 / math.log2(3) + 1 / math.log2(6)
    idcg = 1 / math.log2(2) + 1 / math.log2(3) + 1 / math.log2(4)
    assert ndcg_at_k(RANKING, GOLDEN, 5) == pytest.approx(dcg / idcg)


def test_ndcg_agrees_with_sklearn():
    """Cross-check against an independent implementation.

    sklearn needs dense relevance/score vectors and has no notion of an undefined score, so
    it cannot replace the function under test — but where both are defined they must agree.
    """
    relevance = [[1.0 if chunk_id in GOLDEN else 0.0 for chunk_id in RANKING]]
    scores = [[float(len(RANKING) - i) for i in range(len(RANKING))]]  # descending = the order

    assert ndcg_at_k(RANKING, GOLDEN & set(RANKING), 5) == pytest.approx(
        ndcg_score(relevance, scores, k=5)
    )


def test_ndcg_is_one_for_a_perfect_ranking():
    assert ndcg_at_k(["b", "e"], {"b", "e"}, 5) == pytest.approx(1.0)


def test_ndcg_notices_a_relevant_chunk_sliding_down():
    good = ndcg_at_k(["b", "x", "y", "z2", "w"], {"b"}, 5)
    worse = ndcg_at_k(["x", "y", "z2", "w", "b"], {"b"}, 5)

    assert good == pytest.approx(1.0)
    assert worse < good
    # …while the order-blind metrics cannot tell the two apart
    assert precision_at_k(["b", "x", "y", "z2", "w"], {"b"}, 5) == precision_at_k(
        ["x", "y", "z2", "w", "b"], {"b"}, 5
    )


# --------------------------------------------------------------------------- #
# Empty golden sets
# --------------------------------------------------------------------------- #


def test_every_metric_is_undefined_without_a_golden_set():
    assert score_case(RANKING, set(), 5) == {
        "hit_rate": None,
        "precision": None,
        "recall": None,
        "mrr": None,
        "ndcg": None,
    }


def test_aggregate_excludes_undefined_cases_and_counts_them():
    summary = aggregate(
        [
            score_case(RANKING, {"a"}, 5),  # hit at rank 1
            score_case(RANKING, {"e"}, 5),  # hit at rank 5
            score_case(RANKING, set(), 5),  # out of corpus
        ]
    )

    assert summary["hit_rate"] == pytest.approx(1.0)  # averaged over two, not three
    assert summary["scored"] == 2
    assert summary["undefined"] == 1


def test_aggregate_of_nothing_but_undefined_cases():
    summary = aggregate([score_case(RANKING, set(), 5)])

    assert summary["mrr"] is None
    assert summary["scored"] == 0


# --------------------------------------------------------------------------- #
# The eval set
# --------------------------------------------------------------------------- #

EVALSET = load()
CHUNKS = build_chunks()


def test_every_anchor_resolves_to_a_real_chunk():
    resolved = resolve_all(EVALSET.cases, CHUNKS)

    for case in EVALSET.cases:
        assert (len(resolved[case.id]) > 0) is not case.unanswerable


def test_an_anchor_that_matches_nothing_is_an_error_not_an_empty_set():
    case = EVALSET.cases[0].model_copy(update={"golden_anchors": [("algo-nope", "Pitfalls")]})

    with pytest.raises(ValueError, match="matches no chunk"):
        resolve_golden(case, CHUNKS)


def test_a_split_section_contributes_all_of_its_chunks():
    """Anchors are content, not position: a section split in two is two golden chunks."""
    counts = {case.id: len(resolve_golden(case, CHUNKS)) for case in EVALSET.cases}

    assert any(
        counts[case.id] > len(case.golden_anchors)
        for case in EVALSET.cases
        if case.golden_anchors
    )


def test_the_eval_set_is_tagged_and_spread_across_categories():
    grouped = EVALSET.by_category()

    assert len(EVALSET.cases) >= 20
    assert len(grouped) >= 5
    assert "out_of_corpus" in grouped
    assert set(grouped) <= set(EVALSET.categories), "every category must be documented"


def test_out_of_corpus_cases_have_no_golden_context():
    for case in EVALSET.by_category()["out_of_corpus"]:
        assert case.unanswerable
        assert case.golden_answer, "an unanswerable case still needs a reference answer"


def test_case_ids_are_unique():
    ids = [case.id for case in EVALSET.cases]

    assert len(set(ids)) == len(ids)
