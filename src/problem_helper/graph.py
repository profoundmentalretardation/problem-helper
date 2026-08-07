"""The LangGraph pipeline: screen → baseline run → fix loop → diff → hint loop with tools.

The graph replaces the hand-written loops of the first version. The nodes stay pure
functions of `PipelineState`: they reach the model through `LLMProtocol` and the sandbox
through the injected `runner`, and they never touch the database or the settings object —
`orchestrator` watches the streamed updates and persists them. That is what keeps the
whole pipeline testable without a provider.

    START → screen ─(refused)→ unsafe_input → END
              │
              ▼
           baseline ─┬─(green)→ already_correct → END
                     └─────────→ fix → verify ─┬─(green)→ diff → research ⇄ tools
                                   ▲            └─(retry)→ fix                │
                                   └─(out of attempts)→ fix_failed → END      ▼
      END ← succeeded ←(approved)─ validate ← screen_hint ← write_hint ←───────
                        (retry)──→ research …             (out of attempts)→ hint_rejected

Both loops are edges rather than `for` statements, so every iteration is a checkpoint: a
session that dies mid-flight resumes from the last completed node instead of paying for
the model calls again.

The tool loop is the only place where control flow is up to the model — `research` binds
the tools and `_after_research` simply follows whatever the answer asks for.

**The guardrails are nodes, not wrappers.** `screen` is a node so that a refused request is
a terminal state with an error code rather than an exception, and so that the decision is a
checkpoint like any other. `screen_hint` is a node so a blocked hint re-enters the *existing*
hint retry loop through `_rejection` — the same path the validator's own rejections take,
which is why there is no second retry mechanism to keep in step. The one guardrail that is
not a node of its own is the shield over the fixer's code: `verify` runs it before it runs
the code, and a refusal becomes a failing attempt, so the fix loop retries it unchanged.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass

from langchain_core.messages import (
    AIMessage,
    HumanMessage,
    RemoveMessage,
    SystemMessage,
)
from langchain_core.tools import BaseTool
from langgraph.checkpoint.base import BaseCheckpointSaver
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import REMOVE_ALL_MESSAGES
from langgraph.graph.state import CompiledStateGraph
from langgraph.prebuilt import ToolNode

from . import codediff, codeshield, prompts, safety, sandbox, tracing
from . import tools as tool_registry
from .llm import LLMProtocol
from .sandbox import TestReport
from .schemas import (
    ErrorCode,
    FixResult,
    HintResult,
    Outcome,
    SessionStage,
    ValidationResult,
)
from .state import PipelineState

logger = logging.getLogger(__name__)

Runner = Callable[[str], Awaitable[TestReport]]
"""Runs arbitrary code against this session's tests."""

MAX_TOOL_ROUNDS = 3
"""How many times the hint agent may go back to the tools before it has to answer."""


@dataclass(slots=True, frozen=True)
class GuardConfig:
    """Which guardrail layers are live. All on in the service.

    The switches exist so the attack suite can measure what each layer is worth by running
    the same scenario with one of them off. They are not a supported way to run the service
    — turning a layer off in production is a decision, and it should read like one.
    """

    input_filter: bool = True
    code_shield: bool = True
    output_filter: bool = True

    @classmethod
    def from_settings(cls, settings) -> GuardConfig:
        return cls(
            input_filter=settings.input_filter_enabled,
            code_shield=settings.codeshield_enabled,
            output_filter=settings.output_filter_enabled,
        )


def initial_state(
    *,
    task: str,
    student_code: str,
    tests: list,
    max_fix_attempts: int,
    max_hint_attempts: int,
) -> PipelineState:
    """The input of a fresh run — every counter starts here, not inside a node."""
    return PipelineState(
        task=task,
        student_code=student_code,
        tests=tests,
        max_fix_attempts=max_fix_attempts,
        max_hint_attempts=max_hint_attempts,
        stage=SessionStage.running_tests,
        fix_round=0,
        hint_round=0,
        rejected=[],
        materials=[],
        guardrails=[],
        refused_because="",
        hint_blocked=False,
        outcome=None,
        error_code=None,
    )


def build_graph(
    *,
    llm: LLMProtocol,
    runner: Runner,
    fixer_model: str,
    hint_model: str,
    validator_model: str,
    tools: Sequence[BaseTool] | None = None,
    checkpointer: BaseCheckpointSaver | None = None,
    guards: GuardConfig | None = None,
) -> CompiledStateGraph:
    """Wires the nodes into a compiled graph; `checkpointer` is what makes runs resumable."""
    registered = list(tools if tools is not None else tool_registry.TOOLS)
    guard = guards or GuardConfig()

    # --- nodes ------------------------------------------------------------ #

    async def screen(state: PipelineState) -> dict:
        """Layers 1 and 4 over the request, before a single token is paid for."""
        events: list[dict] = []
        blocked_by = ""

        with tracing.guardrail_span("input_filter", enabled=guard.input_filter) as span:
            verdict = (
                safety.inputs.scan_all(
                    {"task": state["task"], "student_code": state["student_code"]}
                )
                if guard.input_filter
                else safety.InjectionVerdict()
            )
            tracing.set_outputs(span, verdict.as_dict())
        events.append({"layer": "input_filter", **verdict.as_dict()})
        if verdict.blocked:
            blocked_by = f"input filter: {verdict.reason()}"

        with tracing.guardrail_span("code_shield", enabled=guard.code_shield) as span:
            shield = (
                codeshield.scan(state["student_code"])
                if guard.code_shield
                else codeshield.Verdict(allowed=True)
            )
            tracing.set_outputs(span, shield.as_dict())
        events.append({"layer": "code_shield", "target": "student_code", **shield.as_dict()})
        if not shield.allowed and not blocked_by:
            blocked_by = "code shield: " + "; ".join(f.detail for f in shield.findings)

        if blocked_by:
            logger.warning("request refused before execution — %s", blocked_by)
        return {"guardrails": events, "refused_because": blocked_by}

    async def unsafe_input(state: PipelineState) -> dict:
        return {
            "error_code": ErrorCode.unsafe_input,
            "error_message": (
                "The request was refused before anything ran. "
                f"{state['refused_because']}"
            ),
            "stage": SessionStage.done,
        }

    async def baseline(state: PipelineState) -> dict:
        report = await runner(state["student_code"])
        logger.info("baseline: %s/%s tests passed", report.passed_count, report.total)
        return {
            "baseline": report.as_dict(),
            "tests_total": report.total,
            "tests_passed_before": report.passed_count,
        }

    async def already_correct(state: PipelineState) -> dict:
        return {"outcome": Outcome.already_correct, "stage": SessionStage.done}

    async def fix(state: PipelineState) -> dict:
        round_no = state["fix_round"] + 1
        previous = state.get("last_fix_report")
        result: FixResult = await llm.structured(
            model=fixer_model,
            system=prompts.FIXER_SYSTEM,
            user=prompts.fixer_user(
                task=state["task"],
                student_code=state["student_code"],
                tests=state["tests"],
                baseline=sandbox.report_from_dict(state["baseline"]),
                previous_code=state.get("fixed_code") if previous else None,
                previous_report=sandbox.report_from_dict(previous) if previous else None,
            ),
            schema=FixResult,
        )
        return {
            "stage": SessionStage.fixing,
            "fix_round": round_no,
            "fixed_code": result.fixed_code,
            "fix_summary": result.summary,
            "mistakes": result.mistakes,
        }

    async def verify(state: PipelineState) -> dict:
        """Screens the model's own code, then runs it.

        The fixer reads a task statement written by whoever opened the session, so its
        output is as untrusted as its input. A refusal here is a failing attempt with the
        reason in stderr, which is exactly what the fix loop already knows how to retry.
        """
        events: list[dict] = []
        with tracing.guardrail_span("code_shield", enabled=guard.code_shield) as span:
            shield = (
                codeshield.scan(state["fixed_code"])
                if guard.code_shield
                else codeshield.Verdict(allowed=True)
            )
            tracing.set_outputs(span, shield.as_dict())
        events.append({"layer": "code_shield", "target": "fixed_code", **shield.as_dict()})

        if shield.allowed:
            report = await runner(state["fixed_code"])
        else:
            logger.warning("fix attempt %s refused by the code shield", state["fix_round"])
            report = sandbox.refused(state["tests"], shield.for_prompt())

        logger.info(
            "fix attempt %s/%s: %s/%s tests passed",
            state["fix_round"],
            state["max_fix_attempts"],
            report.passed_count,
            report.total,
        )
        record = {
            "attempt": state["fix_round"],
            "passed": report.all_passed,
            "shielded": not shield.allowed,
            "summary": state["fix_summary"],
            "fixed_code": state["fixed_code"],
            "mistakes": [m.model_dump() for m in state["mistakes"]],
            "tests": report.as_dict(),
        }
        return {
            "last_fix_report": report.as_dict(),
            "fix_attempts": [record],
            "guardrails": events,
        }

    async def fix_failed(state: PipelineState) -> dict:
        return {
            "error_code": ErrorCode.fix_failed,
            "error_message": (
                f"Could not produce a working solution in {state['fix_round']} attempt(s)"
            ),
            "stage": SessionStage.done,
        }

    async def diff(state: PipelineState) -> dict:
        return {"diff": codediff.unified(state["student_code"], state["fixed_code"])}

    async def research(state: PipelineState) -> dict:
        """One turn of the hint agent: it either calls tools or announces it is ready."""
        opening: list = []
        if not state.get("research"):
            opening = [
                SystemMessage(prompts.HINT_SYSTEM),
                HumanMessage(
                    prompts.hint_user(
                        task=state["task"],
                        student_code=state["student_code"],
                        fixed_code=state["fixed_code"],
                        diff=state["diff"],
                        mistakes=state["mistakes"],
                        rejected=state.get("rejected"),
                    )
                ),
            ]
        conversation = [*state.get("research", []), *opening]
        answer = await llm.chat(model=hint_model, messages=conversation, tools=registered)
        if answer.tool_calls:
            logger.info(
                "hint agent asks for %s", [call["name"] for call in answer.tool_calls]
            )
        return {"stage": SessionStage.hinting, "research": [*opening, answer]}

    async def write_hint(state: PipelineState) -> dict:
        conversation = state.get("research", [])
        result: HintResult = await llm.structured(
            model=hint_model,
            system=prompts.HINT_SYSTEM,
            user=prompts.HINT_WRITE_INSTRUCTION,
            schema=HintResult,
            history=[m for m in conversation if not isinstance(m, SystemMessage)],
        )
        pulled = tool_registry.read_materials(conversation)
        chosen = set(result.related_material_ids)
        return {
            "hint": result.hint,
            "claimed_material_ids": list(result.related_material_ids),
            "available_material_ids": [ref.id for ref in pulled],
            "materials": [ref for ref in pulled if ref.id in chosen],
        }

    async def screen_hint(state: PipelineState) -> dict:
        """Layer 3 over the hint. A block becomes a rejection, not a failure."""
        with tracing.guardrail_span("output_filter", enabled=guard.output_filter) as span:
            verdict = (
                safety.scan_output(
                    state["hint"],
                    claimed_material_ids=state.get("claimed_material_ids", []),
                    available_material_ids=state.get("available_material_ids", []),
                    fixed_code=state.get("fixed_code", ""),
                )
                if guard.output_filter
                else safety.OutputVerdict()
            )
            tracing.set_outputs(span, verdict.as_dict())

        updates: dict = {
            "guardrails": [{"layer": "output_filter", **verdict.as_dict()}],
            "hint_blocked": verdict.blocked,
        }
        if verdict.blocked:
            updates |= _rejection(state, verdict.issues(), source="output_filter")
        return updates

    async def validate(state: PipelineState) -> dict:
        verdict: ValidationResult = await llm.structured(
            model=validator_model,
            system=prompts.VALIDATOR_SYSTEM,
            user=prompts.validator_user(
                task=state["task"],
                student_code=state["student_code"],
                fixed_code=state["fixed_code"],
                diff=state["diff"],
                hint=state["hint"],
            ),
            schema=ValidationResult,
        )
        logger.info(
            "hint attempt %s/%s: approved=%s, %s remark(s)",
            state["hint_round"] + 1,
            state["max_hint_attempts"],
            verdict.approved,
            len(verdict.issues),
        )
        if verdict.approved:
            return {
                "hint_round": state["hint_round"] + 1,
                "approved": True,
                "issues": [],
                "hint_attempts": [
                    {
                        "attempt": state["hint_round"] + 1,
                        "hint": state["hint"],
                        "approved": True,
                        "issues": [],
                        "rejected_by": None,
                        "materials": [ref.id for ref in state.get("materials", [])],
                    }
                ],
            }
        return _rejection(state, verdict.issues, source="validator")

    async def hint_rejected(state: PipelineState) -> dict:
        issues = "; ".join(state.get("issues") or []) or "no details"
        # Two layers reject hints and the message says which one did it last: "the
        # validator refused it" sent someone reading the output filter's remarks looking
        # for a prompt bug that was not there.
        by = state.get("hint_attempts", [{}])[-1].get("rejected_by") or "validator"
        return {
            "error_code": ErrorCode.hint_rejected,
            "error_message": (
                f"The {by} rejected the hint in {state['hint_round']} attempt(s): {issues}"
            ),
            "stage": SessionStage.done,
        }

    async def succeeded(state: PipelineState) -> dict:
        return {"outcome": Outcome.hint_ready, "stage": SessionStage.done}

    # --- routing ---------------------------------------------------------- #

    def _after_screen(state: PipelineState) -> str:
        return "unsafe_input" if state.get("refused_because") else "baseline"

    def _after_screen_hint(state: PipelineState) -> str:
        if not state.get("hint_blocked"):
            return "validate"
        return "research" if state["hint_round"] < state["max_hint_attempts"] else "hint_rejected"

    def _after_baseline(state: PipelineState) -> str:
        return "already_correct" if _all_passed(state["baseline"]) else "fix"

    def _after_verify(state: PipelineState) -> str:
        if _all_passed(state["last_fix_report"]):
            return "diff"
        return "fix" if state["fix_round"] < state["max_fix_attempts"] else "fix_failed"

    def _after_research(state: PipelineState) -> str:
        last = state["research"][-1]
        wants_tools = isinstance(last, AIMessage) and bool(last.tool_calls)
        return "tools" if wants_tools else "write_hint"

    def _after_tools(state: PipelineState) -> str:
        return "research" if _tool_rounds(state) < MAX_TOOL_ROUNDS else "write_hint"

    def _after_validate(state: PipelineState) -> str:
        if state["approved"]:
            return "succeeded"
        return "research" if state["hint_round"] < state["max_hint_attempts"] else "hint_rejected"

    # --- wiring ----------------------------------------------------------- #

    builder = StateGraph(PipelineState)
    builder.add_node("screen", screen)
    builder.add_node("unsafe_input", unsafe_input)
    builder.add_node("baseline", baseline)
    builder.add_node("already_correct", already_correct)
    builder.add_node("fix", fix)
    builder.add_node("verify", verify)
    builder.add_node("fix_failed", fix_failed)
    builder.add_node("diff", diff)
    builder.add_node("research", research)
    builder.add_node("tools", ToolNode(registered, messages_key="research"))
    builder.add_node("write_hint", write_hint)
    builder.add_node("screen_hint", screen_hint)
    builder.add_node("validate", validate)
    builder.add_node("hint_rejected", hint_rejected)
    builder.add_node("succeeded", succeeded)

    builder.add_edge(START, "screen")
    builder.add_conditional_edges("screen", _after_screen, ["unsafe_input", "baseline"])
    builder.add_conditional_edges("baseline", _after_baseline, ["already_correct", "fix"])
    builder.add_edge("fix", "verify")
    builder.add_conditional_edges("verify", _after_verify, ["diff", "fix", "fix_failed"])
    builder.add_edge("diff", "research")
    builder.add_conditional_edges("research", _after_research, ["tools", "write_hint"])
    builder.add_conditional_edges("tools", _after_tools, ["research", "write_hint"])
    builder.add_edge("write_hint", "screen_hint")
    builder.add_conditional_edges(
        "screen_hint", _after_screen_hint, ["validate", "research", "hint_rejected"]
    )
    builder.add_conditional_edges(
        "validate", _after_validate, ["succeeded", "research", "hint_rejected"]
    )
    for terminal in (
        "unsafe_input",
        "already_correct",
        "fix_failed",
        "hint_rejected",
        "succeeded",
    ):
        builder.add_edge(terminal, END)

    return builder.compile(checkpointer=checkpointer)


def _rejection(state: PipelineState, issues: list[str], *, source: str) -> dict:
    """One rejected hint, whichever layer turned it down.

    Both the validator and the output filter land here so there is a single definition of
    what a rejection costs: one round of the budget, one row in the attempt log, and a
    cleared conversation. A retry starts from a clean context — the rejected hint and the
    remarks go back in as text, which is cheaper than carrying the whole tool exchange.
    """
    round_no = state["hint_round"] + 1
    logger.info("hint attempt %s rejected by the %s: %s", round_no, source, issues)
    return {
        "hint_round": round_no,
        "approved": False,
        "issues": issues,
        "hint_attempts": [
            {
                "attempt": round_no,
                "hint": state["hint"],
                "approved": False,
                "issues": issues,
                "rejected_by": source,
                "materials": [ref.id for ref in state.get("materials", [])],
            }
        ],
        "rejected": [
            *state.get("rejected", []),
            {"hint": state["hint"], "issues": issues},
        ],
        "research": [RemoveMessage(id=REMOVE_ALL_MESSAGES)],
    }


def _all_passed(report: dict) -> bool:
    return bool(report.get("total")) and report["passed"] == report["total"]


def _tool_rounds(state: PipelineState) -> int:
    return sum(
        1
        for message in state.get("research", [])
        if isinstance(message, AIMessage) and message.tool_calls
    )
