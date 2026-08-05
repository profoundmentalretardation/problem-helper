"""The five rank-aware retrieval metrics, computed from chunk ids and nothing else.

No judge, no LLM call, no evaluation library — neither DeepEval nor Ragas ships this
family, and DeepEval's `ContextualPrecisionMetric` / `ContextualRecallMetric` are *judged*
metrics measuring something else entirely. What is here is the textbook definition of each
one, in plain Python, so the arithmetic is inspectable:

- **hit rate@k** — did any relevant chunk make the cut (0 or 1 per case);
- **precision@k** — what fraction of the k returned chunks was relevant;
- **recall@k** — what fraction of the relevant chunks made the cut;
- **MRR** — 1/rank of the first relevant chunk, 0 if none;
- **nDCG@k** — rank-weighted, with binary gains and the ideal ranking as the denominator.

Two invariants the harness depends on:

**Order is the retriever's order.** MRR and nDCG read positions. Scoring a list that has
been reordered for presentation (`pack_for_lim`) measures the packing, and only two of the
five metrics move, so the mistake hides behind three metrics that look fine.

**An empty golden set makes every one of these undefined, not zero.** Out-of-corpus cases
return `None` and are counted separately; averaging them in as zeros would say the retriever
failed on a question it was right to find nothing for.
"""

from __future__ import annotations

import math
from collections.abc import Iterable, Sequence

METRIC_NAMES = ("hit_rate", "precision", "recall", "mrr", "ndcg")


def hit_rate_at_k(retrieved: Sequence[str], relevant: set[str], k: int) -> float | None:
    if not relevant:
        return None
    return 1.0 if any(chunk_id in relevant for chunk_id in retrieved[:k]) else 0.0


def precision_at_k(retrieved: Sequence[str], relevant: set[str], k: int) -> float | None:
    if not relevant:
        return None
    top = retrieved[:k]
    if not top:
        return 0.0
    # The denominator is k, not len(top): a retriever that returns 2 chunks when 5 were
    # asked for has not earned the precision of a retriever that returned 5 good ones.
    return sum(chunk_id in relevant for chunk_id in top) / k


def recall_at_k(retrieved: Sequence[str], relevant: set[str], k: int) -> float | None:
    if not relevant:
        return None
    return len(set(retrieved[:k]) & relevant) / len(relevant)


def reciprocal_rank(retrieved: Sequence[str], relevant: set[str], k: int | None = None) -> float | None:
    """1 / rank of the first relevant chunk. `k` truncates the list first."""
    if not relevant:
        return None
    top = retrieved[:k] if k is not None else retrieved
    for rank, chunk_id in enumerate(top, start=1):
        if chunk_id in relevant:
            return 1.0 / rank
    return 0.0


def ndcg_at_k(retrieved: Sequence[str], relevant: set[str], k: int) -> float | None:
    """Binary-gain nDCG with the standard log2(rank + 1) discount."""
    if not relevant:
        return None
    gains = [1.0 if chunk_id in relevant else 0.0 for chunk_id in retrieved[:k]]
    dcg = sum(gain / math.log2(rank + 1) for rank, gain in enumerate(gains, start=1))
    ideal_hits = min(len(relevant), k)
    idcg = sum(1.0 / math.log2(rank + 1) for rank in range(1, ideal_hits + 1))
    return dcg / idcg if idcg else None


def score_case(retrieved: Sequence[str], relevant: set[str], k: int) -> dict[str, float | None]:
    """All five metrics for one case at one k."""
    return {
        "hit_rate": hit_rate_at_k(retrieved, relevant, k),
        "precision": precision_at_k(retrieved, relevant, k),
        "recall": recall_at_k(retrieved, relevant, k),
        "mrr": reciprocal_rank(retrieved, relevant, k),
        "ndcg": ndcg_at_k(retrieved, relevant, k),
    }


def aggregate(scores: Iterable[dict[str, float | None]]) -> dict[str, float | int | None]:
    """Mean per metric over the cases where it is defined, plus how many were skipped."""
    rows = list(scores)
    summary: dict[str, float | int | None] = {}
    for name in METRIC_NAMES:
        defined = [row[name] for row in rows if row.get(name) is not None]
        summary[name] = sum(defined) / len(defined) if defined else None
    summary["scored"] = sum(1 for row in rows if row.get("hit_rate") is not None)
    summary["undefined"] = sum(1 for row in rows if row.get("hit_rate") is None)
    return summary
