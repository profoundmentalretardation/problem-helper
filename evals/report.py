"""Markdown rendering for the evaluation runs.

Kept apart from the scorers so a number can be re-formatted without touching the code that
produced it — and so the tables in the README are generated, never hand-typed.
"""

from __future__ import annotations

from collections.abc import Sequence


def cell(value: float | None, digits: int = 3) -> str:
    if value is None:
        return "—"
    if isinstance(value, int):
        return str(value)
    return f"{value:.{digits}f}"


def table(headers: Sequence[str], rows: Sequence[Sequence[object]]) -> str:
    """A GitHub-flavoured markdown table; values are formatted by the caller."""
    lines = [
        "| " + " | ".join(str(h) for h in headers) + " |",
        "|" + "|".join("---" for _ in headers) + "|",
    ]
    lines += ["| " + " | ".join(str(c) for c in row) + " |" for row in rows]
    return "\n".join(lines)


def metric_row(label: str, summary: dict) -> list[str]:
    return [
        label,
        cell(summary["hit_rate"]),
        cell(summary["precision"]),
        cell(summary["recall"]),
        cell(summary["mrr"]),
        cell(summary["ndcg"]),
    ]


METRIC_HEADERS = ("run", "hit@k", "precision@k", "recall@k", "MRR", "nDCG@k")
