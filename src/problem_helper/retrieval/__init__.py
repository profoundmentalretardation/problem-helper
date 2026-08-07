"""Hybrid retrieval over the study library.

The public surface is `RetrievalService` (constructed explicitly by the evaluation harness,
so a run's parameters are part of the run) and `get_retriever()` (the lazily-built singleton
the agent tool uses, so a process pays for the ONNX models only if a hint actually searches).

`set_retriever()` exists for tests and for wiring a pre-warmed instance at app startup.
"""

from __future__ import annotations

from pathlib import Path

from .chunking import Chunk, build_chunks
from .fusion import reciprocal_rank_fusion
from .packing import pack_for_lim
from .service import Hit, RetrievalParams, RetrievalService

__all__ = [
    "Chunk",
    "Hit",
    "RetrievalParams",
    "RetrievalService",
    "build_chunks",
    "get_retriever",
    "pack_for_lim",
    "reciprocal_rank_fusion",
    "set_retriever",
]

_retriever: RetrievalService | None = None


def get_retriever() -> RetrievalService:
    """The process-wide retriever, built on first use from the settings."""
    global _retriever
    if _retriever is None:
        from ..config import get_settings

        settings = get_settings()
        _retriever = RetrievalService(
            params=RetrievalParams(
                top_k=settings.retrieval_top_k,
                candidates=settings.retrieval_candidates,
                rerank_depth=settings.retrieval_rerank_depth,
                rrf_k=settings.retrieval_rrf_k,
                rerank=settings.retrieval_rerank,
            ),
            embed_model=settings.retrieval_embed_model,
            rerank_model=settings.retrieval_rerank_model,
            cache_dir=Path(settings.retrieval_cache_dir),
        )
    return _retriever


def set_retriever(retriever: RetrievalService | None) -> None:
    """Install (or clear, with `None`) the singleton."""
    global _retriever
    _retriever = retriever
