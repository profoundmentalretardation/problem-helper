"""The explicit state schema of the LangGraph pipeline.

Every node reads and writes this `TypedDict` and nothing else — no ad-hoc dictionaries and
no hidden globals. The two reducers are what make the state safe to write concurrently and
cheap to stream: `research` accumulates the tool-calling conversation through
`add_messages`, and the two attempt logs append with `operator.add`, so a streamed update
carries exactly the records the node has just produced.
"""

from __future__ import annotations

import operator
from typing import Annotated, TypedDict

from langchain_core.messages import AnyMessage
from langgraph.graph.message import add_messages

from .schemas import ErrorCode, MaterialRef, Mistake, Outcome, SessionStage, TestCase


class RejectedHint(TypedDict):
    """A hint the validator turned down, fed back into the next generation."""

    hint: str
    issues: list[str]


class PipelineState(TypedDict, total=False):
    """State of one session: request, both loops and the verdict.

    Keys are filled in as the graph advances; `total=False` keeps every node free to return
    only its own slice.
    """

    # --- request ---------------------------------------------------------- #
    task: str
    student_code: str
    tests: list[TestCase]
    max_fix_attempts: int
    max_hint_attempts: int

    # --- progress --------------------------------------------------------- #
    stage: SessionStage

    # --- baseline run ----------------------------------------------------- #
    baseline: dict
    tests_total: int
    tests_passed_before: int

    # --- loop 1: repair --------------------------------------------------- #
    fix_round: int
    fixed_code: str
    fix_summary: str
    mistakes: list[Mistake]
    last_fix_report: dict
    fix_attempts: Annotated[list[dict], operator.add]
    diff: str

    # --- loop 2: hint ----------------------------------------------------- #
    hint_round: int
    research: Annotated[list[AnyMessage], add_messages]
    hint: str
    materials: list[MaterialRef]
    approved: bool
    issues: list[str]
    rejected: list[RejectedHint]
    hint_attempts: Annotated[list[dict], operator.add]

    # --- verdict ---------------------------------------------------------- #
    outcome: Outcome | None
    error_code: ErrorCode | None
    error_message: str
