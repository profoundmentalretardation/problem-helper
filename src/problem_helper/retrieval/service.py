"""The hybrid retriever: dense + BM25 → RRF → optional cross-encoder rerank.

    query ─┬─► dense  (bge-small, cosine)  ─ top 20 ─┐
           └─► BM25   (Okapi, k1=1.5)      ─ top 20 ─┴─► RRF (k=60) ─ top 20 ─►
                                                        cross-encoder ─ top k ─► hits

Depth is deliberate. Each retriever returns 20 candidates, RRF fuses them and keeps 20, and
the reranker scores those 20 down to the 5 that are returned. Retrieving 4× the final k is
the standard over-fetch: the cross-encoder can only reorder what fusion handed it, so
anything the recall stage misses is lost for good, while a candidate that fusion ranked 14th
and the reranker likes can still surface.

`search(..., rerank=False)` skips the last stage and returns the fused order. That is a
parameter rather than a separate code path so the evaluation harness measures the pipeline
the service actually runs.

The returned order is **retriever order**. `packing.pack_for_lim` is applied where the text
is rendered for a model, never here.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, replace
from pathlib import Path

from .chunking import Chunk, build_chunks
from .dense import DEFAULT_MODEL as DEFAULT_EMBED_MODEL
from .dense import DenseIndex
from .fusion import reciprocal_rank_fusion
from .lexical import Bm25Index
from .rerank import DEFAULT_MODEL as DEFAULT_RERANK_MODEL
from .rerank import CrossEncoderReranker

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class RetrievalParams:
    """Every knob of the pipeline in one object, so a run can be reproduced from a record."""

    top_k: int = 5
    candidates: int = 20
    """How many chunks each retriever contributes to the fusion."""
    rerank_depth: int = 20
    """How many fused candidates the cross-encoder scores."""
    rrf_k: int = 60
    rerank: bool = True

    def as_dict(self) -> dict:
        return {
            "top_k": self.top_k,
            "candidates": self.candidates,
            "rerank_depth": self.rerank_depth,
            "rrf_k": self.rrf_k,
            "rerank": self.rerank,
        }


@dataclass(frozen=True, slots=True)
class Hit:
    """One retrieved chunk with the trail of how it got here."""

    chunk: Chunk
    score: float
    stage: str
    """Which stage produced `score`: "rerank" or "rrf"."""
    dense_rank: int | None = None
    bm25_rank: int | None = None
    rrf_rank: int | None = None

    def as_dict(self) -> dict:
        return {
            "chunk_id": self.chunk.id,
            "material_id": self.chunk.material_id,
            "heading": self.chunk.heading,
            "score": round(self.score, 5),
            "stage": self.stage,
            "dense_rank": self.dense_rank,
            "bm25_rank": self.bm25_rank,
            "rrf_rank": self.rrf_rank,
        }


class RetrievalService:
    """Owns the indexes; one instance per process is plenty."""

    def __init__(
        self,
        chunks: list[Chunk] | None = None,
        *,
        params: RetrievalParams | None = None,
        embed_model: str = DEFAULT_EMBED_MODEL,
        rerank_model: str = DEFAULT_RERANK_MODEL,
        cache_dir: Path | None = None,
    ) -> None:
        self.chunks = chunks if chunks is not None else build_chunks()
        self.params = params or RetrievalParams()
        started = time.perf_counter()
        self.dense = DenseIndex(self.chunks, model=embed_model, cache_dir=cache_dir)
        self.bm25 = Bm25Index(self.chunks)
        self.reranker = CrossEncoderReranker(model=rerank_model)
        logger.info(
            "retrieval ready: %s chunks from %s materials in %.0f ms",
            len(self.chunks),
            len({c.material_id for c in self.chunks}),
            (time.perf_counter() - started) * 1000,
        )

    # ------------------------------------------------------------------ #

    def search(
        self,
        query: str,
        *,
        k: int | None = None,
        rerank: bool | None = None,
    ) -> list[Hit]:
        """Top-k chunks in retriever order, best first."""
        params = self._params(k=k, rerank=rerank)
        stages = self.explain(query, params=params)
        return stages["rerank" if params.rerank else "fused"]

    def explain(self, query: str, *, params: RetrievalParams | None = None) -> dict[str, list[Hit]]:
        """Every stage's ranking for one query — what the before/after study reads.

        Returns `dense`, `bm25`, `fused` and (when reranking is on) `rerank`, each already
        cut to the stage's own depth.
        """
        params = params or self.params
        dense = self.dense.search(query, params.candidates)
        lexical = self.bm25.search(query, params.candidates)

        dense_rank = {chunk.id: i for i, (chunk, _) in enumerate(dense, start=1)}
        bm25_rank = {chunk.id: i for i, (chunk, _) in enumerate(lexical, start=1)}

        def trail(chunk: Chunk) -> dict:
            return {
                "dense_rank": dense_rank.get(chunk.id),
                "bm25_rank": bm25_rank.get(chunk.id),
            }

        fused_pairs = reciprocal_rank_fusion(
            [[c for c, _ in dense], [c for c, _ in lexical]],
            k=params.rrf_k,
            limit=max(params.rerank_depth, params.top_k),
        )
        fused = [
            Hit(chunk=chunk, score=score, stage="rrf", rrf_rank=rank, **trail(chunk))
            for rank, (chunk, score) in enumerate(fused_pairs, start=1)
        ]

        stages: dict[str, list[Hit]] = {
            "dense": [
                Hit(chunk=c, score=s, stage="dense", **trail(c)) for c, s in dense
            ][: params.top_k],
            "bm25": [
                Hit(chunk=c, score=s, stage="bm25", **trail(c)) for c, s in lexical
            ][: params.top_k],
            "fused": fused[: params.top_k],
        }
        if params.rerank:
            scored = self.reranker.rerank(query, [hit.chunk for hit in fused])
            by_id = {hit.chunk.id: hit for hit in fused}
            stages["rerank"] = [
                replace(by_id[chunk.id], score=score, stage="rerank")
                for chunk, score in scored
            ][: params.top_k]
        return stages

    def _params(self, *, k: int | None, rerank: bool | None) -> RetrievalParams:
        changes: dict = {}
        if k is not None:
            changes["top_k"] = k
        if rerank is not None:
            changes["rerank"] = rerank
        return replace(self.params, **changes) if changes else self.params
