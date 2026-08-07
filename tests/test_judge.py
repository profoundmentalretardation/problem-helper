"""Tests for the judged generation scorers.

The judge's *opinion* cannot be unit-tested — the arithmetic on top of it can, and that is
where every scorer bug this harness could have lives: a ratio over the wrong denominator, a
rank weighting that ignores rank, a cache key that collides. `FakeLLM` supplies canned
verdicts so the tests run with no provider, and two real cases from the eval set are used as
fixtures so the scorers are exercised on the shape of data they will actually see.
"""

import json

import pytest
from conftest import FakeLLM

from evals.dataset import load
from evals.judge import (
    ClaimVerdict,
    ContextRecallAnswer,
    ContextVerdict,
    FaithfulnessAnswer,
    Judge,
    RelevanceAnswer,
    RequirementVerdict,
    ResponseCache,
    SentenceVerdict,
)

EVALSET = load()
GROUNDED_CASE = next(c for c in EVALSET.cases if c.id == "heapq-max-heap")
OUT_OF_CORPUS_CASE = next(c for c in EVALSET.cases if c.id == "red-black-tree")

CONTEXTS = [
    (
        "Heaps and priority queues — Max-heap by negation\nheapq only implements a "
        "min-heap. For a max-heap, store negated keys and negate again on the way out."
    ),
    "Sorting with a key function — Stability\nPython's sort is stable.",
]


def judge_with(script, cache=None) -> Judge:
    return Judge(FakeLLM(script=script), model="judge-model", cache=cache or ResponseCache(_nowhere()))


def _nowhere():
    """A cache path that does not exist, so nothing is loaded and nothing is written."""
    import pathlib
    import tempfile

    return pathlib.Path(tempfile.mkdtemp()) / "cache.json"


def claims(*supported: bool) -> FaithfulnessAnswer:
    return FaithfulnessAnswer(
        claims=[
            ClaimVerdict(claim=f"claim {i}", supported=value, reason="…")
            for i, value in enumerate(supported)
        ]
    )


# --------------------------------------------------------------------------- #
# Faithfulness
# --------------------------------------------------------------------------- #


async def test_faithfulness_is_the_supported_fraction():
    judge = judge_with({FaithfulnessAnswer: [claims(True, True, False, True)]})

    score = await judge.faithfulness(
        question=GROUNDED_CASE.query, answer="negate on the way in and out", contexts=CONTEXTS
    )

    assert score["score"] == pytest.approx(0.75)
    assert score["claims"] == 4
    assert score["unsupported"] == ["claim 2"]


async def test_faithfulness_of_an_answer_that_claims_nothing_is_one():
    """An answer that only says "the library does not cover this" invents nothing."""
    judge = judge_with({FaithfulnessAnswer: [FaithfulnessAnswer(claims=[])]})

    score = await judge.faithfulness(
        question=OUT_OF_CORPUS_CASE.query, answer="not covered", contexts=CONTEXTS
    )

    assert score["score"] == 1.0


async def test_faithfulness_bottoms_out_at_zero():
    judge = judge_with({FaithfulnessAnswer: [claims(False, False)]})

    score = await judge.faithfulness(question="q", answer="invented", contexts=CONTEXTS)

    assert score["score"] == 0.0


# --------------------------------------------------------------------------- #
# Answer relevance
# --------------------------------------------------------------------------- #


def requirements(*satisfied: bool, evasive: bool = False) -> RelevanceAnswer:
    return RelevanceAnswer(
        requirements=[
            RequirementVerdict(requirement=f"req {i}", satisfied=value, reason="…")
            for i, value in enumerate(satisfied)
        ],
        evasive=evasive,
    )


async def test_answer_relevance_is_the_satisfied_fraction():
    judge = judge_with({RelevanceAnswer: [requirements(True, False)]})

    score = await judge.answer_relevance(question=GROUNDED_CASE.query, answer="…")

    assert score["score"] == pytest.approx(0.5)
    assert score["missed"] == ["req 1"]


async def test_an_evasive_answer_is_halved():
    judge = judge_with({RelevanceAnswer: [requirements(True, True, evasive=True)]})

    score = await judge.answer_relevance(question="q", answer="waffle")

    assert score["score"] == pytest.approx(0.5)
    assert score["evasive"] is True


async def test_answer_relevance_never_sees_the_context():
    """Relevance is about the question; a judge shown the passages grades grounding."""
    llm = FakeLLM(script={RelevanceAnswer: [requirements(True)]})
    judge = Judge(llm, model="judge-model", cache=ResponseCache(_nowhere()))

    await judge.answer_relevance(question="q", answer="a")

    assert "Max-heap by negation" not in llm.calls[0].user


# --------------------------------------------------------------------------- #
# Context precision — the rank weighting is done here, not by the judge
# --------------------------------------------------------------------------- #


def verdicts(*useful: bool) -> list[ContextVerdict]:
    return [ContextVerdict(useful=value, reason="…") for value in useful]


async def test_context_precision_rewards_useful_chunks_at_the_top():
    top = judge_with({ContextVerdict: verdicts(True, False, False)})
    bottom = judge_with({ContextVerdict: verdicts(False, False, True)})

    high = await top.context_precision(question="q", contexts=["a", "b", "c"])
    low = await bottom.context_precision(question="q", contexts=["a", "b", "c"])

    assert high["score"] == pytest.approx(1.0)
    assert low["score"] == pytest.approx(1 / 3)
    assert high["useful"] == low["useful"] == 1


async def test_context_precision_is_zero_when_nothing_is_useful():
    judge = judge_with({ContextVerdict: verdicts(False, False)})

    score = await judge.context_precision(question="q", contexts=["a", "b"])

    assert score["score"] == 0.0
    assert score["of"] == 2
    assert len(score["rejected"]) == 2


async def test_context_precision_judges_each_passage_on_its_own():
    """One call per passage: the judge never sees the ranking it would otherwise anchor on."""
    llm = FakeLLM(script={ContextVerdict: verdicts(True, True, True)})
    judge = Judge(llm, model="judge-model", cache=ResponseCache(_nowhere()))

    await judge.context_precision(question="q", contexts=["a", "b", "c"])

    assert len(llm.calls) == 3
    assert all("[2]" not in call.user for call in llm.calls)


# --------------------------------------------------------------------------- #
# Context recall
# --------------------------------------------------------------------------- #


async def test_context_recall_is_the_attributable_fraction():
    judge = judge_with(
        {
            ContextRecallAnswer: [
                ContextRecallAnswer(
                    sentences=[
                        SentenceVerdict(sentence="s1", attributable=True, reason="…"),
                        SentenceVerdict(sentence="s2", attributable=False, reason="…"),
                    ]
                )
            ]
        }
    )

    score = await judge.context_recall(
        golden_answer=GROUNDED_CASE.golden_answer, contexts=CONTEXTS
    )

    assert score["score"] == pytest.approx(0.5)
    assert score["unsupported"] == ["s2"]


# --------------------------------------------------------------------------- #
# Cache
# --------------------------------------------------------------------------- #


async def test_a_repeated_judgement_costs_no_call():
    llm = FakeLLM(script={FaithfulnessAnswer: [claims(True, False)]})
    judge = Judge(llm, model="judge-model", cache=ResponseCache(_nowhere()))

    first = await judge.faithfulness(question="q", answer="a", contexts=CONTEXTS)
    second = await judge.faithfulness(question="q", answer="a", contexts=CONTEXTS)

    assert first == second
    assert len(llm.calls) == 1
    assert judge.cache.hits == 1


async def test_the_cache_key_separates_metrics_models_and_payloads():
    a = ResponseCache.key("m1", "faithfulness", "payload")
    assert a != ResponseCache.key("m2", "faithfulness", "payload")
    assert a != ResponseCache.key("m1", "context_recall", "payload")
    assert a != ResponseCache.key("m1", "faithfulness", "other payload")


async def test_the_cache_survives_a_round_trip_to_disk(tmp_path):
    path = tmp_path / "cache.json"
    llm = FakeLLM(script={FaithfulnessAnswer: [claims(True)]})
    judge = Judge(llm, model="judge-model", cache=ResponseCache(path))
    await judge.faithfulness(question="q", answer="a", contexts=CONTEXTS)
    judge.cache.save()

    reloaded = Judge(FakeLLM(script={}), model="judge-model", cache=ResponseCache(path))
    score = await reloaded.faithfulness(question="q", answer="a", contexts=CONTEXTS)

    assert score["score"] == 1.0
    assert json.loads(path.read_text())  # the file is real JSON, inspectable by hand
