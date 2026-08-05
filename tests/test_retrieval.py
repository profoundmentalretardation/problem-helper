"""Tests for the parts of the retrieval layer that need no model.

Chunking, BM25 and RRF are pure functions of the corpus, so they are tested exactly; the
dense index and the reranker are covered end to end by the evaluation harness instead of by
loading ONNX into the unit suite.
"""

import pytest
from conftest import chunk

from problem_helper import materials
from problem_helper.retrieval import reciprocal_rank_fusion
from problem_helper.retrieval.chunking import SPLIT_THRESHOLD, build_chunks
from problem_helper.retrieval.dense import corpus_digest
from problem_helper.retrieval.lexical import Bm25Index, tokenize
from problem_helper.retrieval.packing import pack_for_lim

CHUNKS = build_chunks()


# --------------------------------------------------------------------------- #
# Chunking
# --------------------------------------------------------------------------- #


def test_every_material_produces_at_least_one_chunk():
    assert {c.material_id for c in CHUNKS} == {m.id for m in materials.all()}


def test_chunk_ids_are_unique():
    ids = [c.id for c in CHUNKS]
    assert len(set(ids)) == len(ids)


def test_short_sections_are_not_split():
    """A section under the threshold keeps its text verbatim — no lost sentences."""
    by_anchor: dict[tuple[str, str], list] = {}
    for c in CHUNKS:
        by_anchor.setdefault(c.anchor, []).append(c)
    for section in materials.sections():
        if len(section.text) <= SPLIT_THRESHOLD:
            pieces = by_anchor[(section.material_id, section.heading)]
            assert any(piece.text == section.text for piece in pieces)


def test_long_sections_are_split_with_overlap():
    long_sections = [s for s in materials.sections() if len(s.text) > SPLIT_THRESHOLD]
    assert long_sections, "the corpus should exercise the size-based splitter"
    anchor = (long_sections[0].material_id, long_sections[0].heading)
    parts = [c for c in CHUNKS if c.anchor == anchor]
    assert len(parts) > 1


def test_indexed_text_carries_the_title_and_heading():
    piece = CHUNKS[0]
    assert piece.title in piece.indexed_text
    assert piece.heading in piece.indexed_text


def test_corpus_digest_changes_with_the_text():
    other = [*CHUNKS[:-1]]
    assert corpus_digest("m", CHUNKS) != corpus_digest("m", other)
    assert corpus_digest("m", CHUNKS) == corpus_digest("m", CHUNKS)
    assert corpus_digest("m", CHUNKS) != corpus_digest("other-model", CHUNKS)


# --------------------------------------------------------------------------- #
# BM25 tokenizer — the reason the index is hand-rolled
# --------------------------------------------------------------------------- #


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("deque.popleft()", ["deque", "popleft"]),
        ("BM25", ["bm25"]),
        ("two pointers", ["two", "pointer"]),
        ("class", ["class"]),  # never strips the -s of a word ending in -ss
        ("axis bonus", ["axis", "bonus"]),  # nor after -i- or -u-
    ],
)
def test_tokenizer_rules(text, expected):
    assert tokenize(text) == expected


def test_identifiers_are_indexed_whole_and_in_parts():
    assert tokenize("bisect_left") == ["bisect_left", "bisect", "left"]


def test_bm25_finds_an_exact_identifier():
    index = Bm25Index(CHUNKS)

    top = index.search("bisect_left", limit=3)

    assert top, "an identifier that appears in the corpus must be findable"
    assert top[0][0].material_id == "algo-binary-search"


def test_bm25_returns_nothing_for_an_out_of_corpus_term():
    assert Bm25Index(CHUNKS).search("quantum entanglement holography", limit=5) == []


def test_bm25_scores_are_positive_and_descending():
    scores = [score for _, score in Bm25Index(CHUNKS).search("sliding window", limit=5)]

    assert all(s > 0 for s in scores)
    assert scores == sorted(scores, reverse=True)


# --------------------------------------------------------------------------- #
# Fusion
# --------------------------------------------------------------------------- #


def test_rrf_rewards_agreement_over_a_single_first_place():
    """The consensus bias: found by both beats found first by one."""
    a, b, c = chunk("algo-sets"), chunk("algo-heaps"), chunk("algo-greedy")

    fused = reciprocal_rank_fusion([[a, b], [c, b]], k=60)

    # b is second in both rankings; a and c are each first in exactly one
    assert fused[0][0].id == b.id
    assert fused[0][1] == pytest.approx(2 / 62)


def test_rrf_does_not_reward_agreement_unconditionally():
    """Two seconds do NOT beat a first plus a third — 1/61 + 1/63 > 2/62.

    Worth pinning: the fusion is convex, so "appears twice" is not by itself decisive, and
    a reading of RRF that assumes otherwise mis-explains the ranking.
    """
    a, b, c = chunk("algo-sets"), chunk("algo-heaps"), chunk("algo-greedy")

    fused = reciprocal_rank_fusion([[a, b, c], [c, b, a]], k=60)

    assert {fused[0][0].id, fused[1][0].id} == {a.id, c.id}
    assert fused[2][0].id == b.id


def test_rrf_uses_the_documented_constant():
    a = chunk("algo-sets")

    (only,) = reciprocal_rank_fusion([[a]], k=60)

    assert only[1] == pytest.approx(1 / 61)


def test_rrf_deduplicates_by_chunk_id():
    a, b = chunk("algo-sets"), chunk("algo-heaps")

    fused = reciprocal_rank_fusion([[a, b], [a, b]], k=60)

    assert [c.id for c, _ in fused] == [a.id, b.id]


def test_rrf_respects_the_limit():
    a, b, c = chunk("algo-sets"), chunk("algo-heaps"), chunk("algo-greedy")

    assert len(reciprocal_rank_fusion([[a, b, c]], k=60, limit=2)) == 2


# --------------------------------------------------------------------------- #
# LIM packing
# --------------------------------------------------------------------------- #


def test_pack_for_lim_puts_the_strongest_at_the_edges():
    assert pack_for_lim([1, 2, 3, 4, 5]) == [1, 3, 5, 4, 2]


def test_pack_for_lim_keeps_every_item():
    packed = pack_for_lim(list(range(8)))

    assert sorted(packed) == list(range(8))
    assert packed[0] == 0 and packed[-1] == 1  # best first, second-best last


def test_pack_for_lim_is_a_no_op_for_short_lists():
    assert pack_for_lim([]) == []
    assert pack_for_lim([1]) == [1]
    assert pack_for_lim([1, 2]) == [1, 2]
