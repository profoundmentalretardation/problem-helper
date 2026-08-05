"""Cross-encoder reranking of the fused candidates.

Bi-encoders (the dense index) embed the query and the chunk separately, so the model never
sees them together and similarity is whatever survives the compression into 384 numbers. A
cross-encoder reads `(query, chunk)` as one sequence and scores the pair directly. That is
strictly more informative and strictly more expensive — it is one forward pass per
candidate, which is why it runs over 20 candidates and not over the whole corpus.

`Xenova/ms-marco-MiniLM-L-6-v2` is the ONNX export of the model the course notebooks load
through `sentence-transformers`: 6 layers, ~80 MB, trained on MS MARCO passage ranking. Its
output is an unbounded logit, not a probability — usable for ordering, meaningless as an
absolute threshold.

The model is loaded lazily, so a process that never reranks never pays for it, and
`enabled=False` short-circuits the whole stage. That switch is the seam the evaluation
harness runs both ways.
"""

from __future__ import annotations

import logging

from .chunking import Chunk

logger = logging.getLogger(__name__)

DEFAULT_MODEL = "Xenova/ms-marco-MiniLM-L-6-v2"


class CrossEncoderReranker:
    """Reorders candidates by a cross-encoder score; deterministic for a fixed model."""

    def __init__(self, *, model: str = DEFAULT_MODEL) -> None:
        self._model_id = model
        self._model = None

    def _load(self):
        if self._model is None:
            from fastembed.rerank.cross_encoder import TextCrossEncoder

            logger.info("loading reranker %s", self._model_id)
            self._model = TextCrossEncoder(self._model_id)
        return self._model

    def rerank(self, query: str, candidates: list[Chunk]) -> list[tuple[Chunk, float]]:
        if not candidates:
            return []
        scores = list(self._load().rerank(query, [c.indexed_text for c in candidates]))
        ranked = sorted(
            zip(candidates, scores, strict=True),
            key=lambda pair: -pair[1],
        )
        return [(chunk, float(score)) for chunk, score in ranked]
