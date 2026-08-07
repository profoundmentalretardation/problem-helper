"""Dense vector search over the chunks — the semantic half of the hybrid retriever.

`BAAI/bge-small-en-v1.5` served through `fastembed`, i.e. the same model the course
notebooks load through `sentence-transformers`, but on onnxruntime: 384 dimensions, ~67 MB
of weights and no torch in the dependency tree of a service whose actual job is running
student code.

There is no vector database. The corpus is ~180 chunks, so the index is one `(n, 384)`
float32 matrix and a query is a single matrix-vector product — exact search in well under a
millisecond, against an approximate index that would need a server, a persistence format
and a nearest-neighbour parameter to defend. The matrix is cached on disk keyed by a digest
of (model id, chunk ids, chunk text), so it is recomputed exactly when the corpus changes.

BGE models are trained with an asymmetric instruction: queries get a prefix, passages do
not. `fastembed` applies it in `query_embed`, which is why queries and documents go through
different calls here.
"""

from __future__ import annotations

import hashlib
import logging
from pathlib import Path

import numpy as np

from .chunking import Chunk

logger = logging.getLogger(__name__)

DEFAULT_MODEL = "BAAI/bge-small-en-v1.5"


def corpus_digest(model: str, chunks: list[Chunk]) -> str:
    """Identity of an embedding matrix: the model plus the exact text that went into it."""
    digest = hashlib.sha256(model.encode())
    for chunk in chunks:
        digest.update(chunk.id.encode())
        digest.update(chunk.indexed_text.encode())
    return digest.hexdigest()[:16]


class DenseIndex:
    """Cosine search over an in-memory matrix of unit-length chunk vectors."""

    def __init__(
        self,
        chunks: list[Chunk],
        *,
        model: str = DEFAULT_MODEL,
        cache_dir: Path | None = None,
    ) -> None:
        self._chunks = chunks
        self._model_id = model
        self._cache_dir = cache_dir
        self._embedder = None  # loaded on first use, and skipped entirely on a cache hit
        self._matrix = self._load_or_embed()

    # ------------------------------------------------------------------ #

    def _load_or_embed(self) -> np.ndarray:
        path = self._cache_path()
        if path is not None and path.exists():
            matrix = np.load(path)["vectors"]
            if matrix.shape[0] == len(self._chunks):
                logger.info("dense index: %s vectors from cache", matrix.shape[0])
                return matrix
        matrix = self._embed([c.indexed_text for c in self._chunks])
        if path is not None:
            path.parent.mkdir(parents=True, exist_ok=True)
            np.savez_compressed(path, vectors=matrix)
        logger.info("dense index: embedded %s chunks with %s", len(self._chunks), self._model_id)
        return matrix

    def _cache_path(self) -> Path | None:
        if self._cache_dir is None:
            return None
        return self._cache_dir / f"dense-{corpus_digest(self._model_id, self._chunks)}.npz"

    def _load_embedder(self):
        if self._embedder is None:
            from fastembed import TextEmbedding  # imported late: loading onnx is not free

            self._embedder = TextEmbedding(self._model_id)
        return self._embedder

    def _embed(self, texts: list[str]) -> np.ndarray:
        vectors = np.asarray(list(self._load_embedder().embed(texts)), dtype=np.float32)
        return _normalize(vectors)

    def embed_query(self, query: str) -> np.ndarray:
        """Queries take the BGE instruction prefix; `query_embed` adds it."""
        raw = next(iter(self._load_embedder().query_embed([query])))
        vector = np.asarray(raw, dtype=np.float32)
        return _normalize(vector[None, :])[0]

    # ------------------------------------------------------------------ #

    def search(self, query: str, limit: int) -> list[tuple[Chunk, float]]:
        if not self._chunks:
            return []
        scores = self._matrix @ self.embed_query(query)
        top = np.argsort(-scores)[:limit]
        return [(self._chunks[i], float(scores[i])) for i in top]


def _normalize(matrix: np.ndarray) -> np.ndarray:
    norms = np.linalg.norm(matrix, axis=1, keepdims=True)
    return matrix / np.maximum(norms, 1e-12)
