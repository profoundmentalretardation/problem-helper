"""Reciprocal Rank Fusion of several rankings.

RRF scores a document by `sum(1 / (k + rank))` over the rankings that contain it. It uses
**ranks, not scores**, which is the whole point: a BM25 score of 9.7 and a cosine similarity
of 0.61 are not on a comparable scale and any weighted sum of them is a fudge factor in
disguise. Ranks are.

`k = 60` is the constant from Cormack et al. (2009) and the default in every implementation
that followed, including LangChain's `EnsembleRetriever`. It is large relative to the depth
we fuse (20 per retriever), which flattens the curve: rank 1 contributes 1/61 and rank 10
contributes 1/70, so a document that both retrievers place in their top ten outranks one
that a single retriever puts first. That consensus bias is what we want from fusion — the
reranker, not the fusion, is responsible for fine-grained ordering at the top.
"""

from __future__ import annotations

from collections.abc import Sequence

from .chunking import Chunk

DEFAULT_K = 60


def reciprocal_rank_fusion(
    rankings: Sequence[Sequence[Chunk]],
    *,
    k: int = DEFAULT_K,
    limit: int | None = None,
) -> list[tuple[Chunk, float]]:
    """Fuse ranked chunk lists; ties keep the order of the first ranking that saw the chunk."""
    scores: dict[str, float] = {}
    seen: dict[str, Chunk] = {}
    order: dict[str, int] = {}
    for ranking in rankings:
        for rank, chunk in enumerate(ranking, start=1):
            scores[chunk.id] = scores.get(chunk.id, 0.0) + 1.0 / (k + rank)
            if chunk.id not in seen:
                seen[chunk.id] = chunk
                order[chunk.id] = len(order)
    fused = sorted(scores.items(), key=lambda item: (-item[1], order[item[0]]))
    if limit is not None:
        fused = fused[:limit]
    return [(seen[chunk_id], score) for chunk_id, score in fused]
