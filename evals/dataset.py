"""The evaluation set and how its golden context is resolved.

A case names its golden context by **content anchor** — `(material_id, heading)` — not by
chunk id. Chunk ids are positional (`algo-heaps#3.1`) and move the moment the chunking
parameters change, so an eval set written in chunk ids silently rots into a measurement of
nothing. Anchors are resolved against the live chunk list at load time, and a section that
was split into three chunks contributes all three: retrieving any part of the section that
answers the question is a hit.

Cases whose corpus genuinely cannot answer them carry an **empty** golden set. That is data,
not a gap: the rank-aware metrics are undefined for those cases and the harness reports
their count separately instead of scoring them as zero.
"""

from __future__ import annotations

from pathlib import Path

from pydantic import BaseModel, Field

from problem_helper.retrieval import Chunk

CASES_PATH = Path(__file__).parent / "cases.json"


class EvalCase(BaseModel):
    """One question, its golden context and the answer a good system would give."""

    id: str
    category: str
    query: str
    golden_anchors: list[tuple[str, str]]
    golden_answer: str

    @property
    def unanswerable(self) -> bool:
        return not self.golden_anchors


class EvalSet(BaseModel):
    categories: dict[str, str] = Field(description="failure category → why it is in the set")
    cases: list[EvalCase]

    def by_category(self) -> dict[str, list[EvalCase]]:
        grouped: dict[str, list[EvalCase]] = {}
        for case in self.cases:
            grouped.setdefault(case.category, []).append(case)
        return grouped


def load(path: Path = CASES_PATH) -> EvalSet:
    return EvalSet.model_validate_json(path.read_text(encoding="utf-8"))


def resolve_golden(case: EvalCase, chunks: list[Chunk]) -> set[str]:
    """Anchors → the chunk ids that currently carry that content.

    Raises when an anchor matches nothing: a typo in the eval set would otherwise look like
    a retrieval failure, which is the most expensive kind of bug in an eval harness.
    """
    golden: set[str] = set()
    for anchor in case.golden_anchors:
        matching = {chunk.id for chunk in chunks if chunk.anchor == tuple(anchor)}
        if not matching:
            raise ValueError(f"case {case.id!r}: anchor {anchor} matches no chunk")
        golden |= matching
    return golden


def resolve_all(cases: list[EvalCase], chunks: list[Chunk]) -> dict[str, set[str]]:
    return {case.id: resolve_golden(case, chunks) for case in cases}
