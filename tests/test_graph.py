import pytest
from conftest import FakeLLM, report, sample_tests, tool_turn
from langchain_core.messages import AIMessage, ToolMessage
from langgraph.checkpoint.memory import InMemorySaver

from problem_helper.graph import MAX_TOOL_ROUNDS, build_graph, initial_state
from problem_helper.schemas import (
    ErrorCode,
    FixResult,
    HintResult,
    Mistake,
    Outcome,
    ValidationResult,
)

MISTAKE = Mistake(title="wrong sign", detail="minus instead of plus", line=2)
BROKEN = "print(a - b)"


def fix(code: str) -> FixResult:
    return FixResult(mistakes=[MISTAKE], fixed_code=code, summary="fixed the sign")


def hint(text: str, materials: list[str] | None = None) -> HintResult:
    return HintResult(hint=text, related_material_ids=materials or [])


def verdict(approved: bool, issues: list[str] | None = None) -> ValidationResult:
    return ValidationResult(approved=approved, issues=issues or [])


def llm_with(**script) -> FakeLLM:
    """A fake with sensible defaults for the parts a test does not care about."""
    turns = script.pop("turns", [])
    return FakeLLM(
        {
            FixResult: script.get("fixes", [fix("print(a + b)")]),
            HintResult: script.get("hints", [hint("look at the operator on line 2")]),
            ValidationResult: script.get("verdicts", [verdict(True)]),
        },
        turns=turns,
    )


def runner_returning(*results):
    queue = list(results)

    async def runner(code: str):
        return queue.pop(0) if len(queue) > 1 else queue[0]

    return runner


async def run(llm, runner, *, max_fix=3, max_hint=3, checkpointer=None, thread="t1"):
    graph = build_graph(
        llm=llm,
        runner=runner,
        fixer_model="fixer",
        hint_model="cheap",
        validator_model="strong",
        checkpointer=checkpointer,
    )
    state = initial_state(
        task="sum of two numbers",
        student_code=BROKEN,
        tests=sample_tests(),
        max_fix_attempts=max_fix,
        max_hint_attempts=max_hint,
    )
    return await graph.ainvoke(state, {"configurable": {"thread_id": thread}})


# --------------------------------------------------------------------------- #
# Loop 1: repair
# --------------------------------------------------------------------------- #


async def test_correct_code_skips_both_loops():
    llm = llm_with()

    final = await run(llm, runner_returning(report(passed=True)))

    assert final["outcome"] == Outcome.already_correct
    assert final["tests_passed_before"] == 2
    assert llm.calls == []


async def test_fix_and_hint_happy_path():
    llm = llm_with()

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert final["outcome"] == Outcome.hint_ready
    assert final["fixed_code"] == "print(a + b)"
    assert final["hint"] == "look at the operator on line 2"
    assert final["fix_round"] == 1
    assert final["fix_attempts"][0]["passed"] is True
    assert "-print(a - b)" in final["diff"]
    assert [c.model for c in llm.calls] == ["fixer", "cheap", "strong"]


async def test_second_fix_attempt_gets_previous_failure_in_context():
    llm = llm_with(fixes=[fix("first edit"), fix("print(a + b)")])

    final = await run(
        llm, runner_returning(report(passed=False), report(passed=False), report(passed=True))
    )

    assert final["outcome"] == Outcome.hint_ready
    assert final["fix_round"] == 2
    second_prompt = llm.users_for(FixResult)[1]
    assert "Your previous fix did not pass the tests" in second_prompt
    assert "first edit" in second_prompt


async def test_gives_up_after_max_fix_attempts():
    llm = llm_with(fixes=[fix("still broken")])

    final = await run(llm, runner_returning(report(passed=False)), max_fix=2)

    assert final["error_code"] == ErrorCode.fix_failed
    assert "2 attempt(s)" in final["error_message"]
    assert len(final["fix_attempts"]) == 2
    assert final.get("hint") is None


# --------------------------------------------------------------------------- #
# Loop 2: hint and validation
# --------------------------------------------------------------------------- #


async def test_validator_remarks_go_into_the_next_generation():
    llm = llm_with(
        hints=[hint("check your code"), hint("compare the operator with the statement")],
        verdicts=[verdict(False, ["too vague"]), verdict(True)],
    )

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert final["outcome"] == Outcome.hint_ready
    assert final["hint"] == "compare the operator with the statement"
    assert len(final["hint_attempts"]) == 2
    retry_context = llm.chat_calls[-1].messages[-1].content
    assert "Rejected hint #1" in retry_context
    assert "too vague" in retry_context


async def test_retry_starts_from_a_clean_conversation():
    llm = llm_with(
        turns=[tool_turn("list_material_topics"), AIMessage("ready")],
        verdicts=[verdict(False, ["too vague"]), verdict(True)],
    )

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert final["outcome"] == Outcome.hint_ready
    # the second round opens with system + user only, the first round's tool traffic is gone
    assert not any(isinstance(m, ToolMessage) for m in final["research"])


async def test_fails_when_the_validator_never_approves():
    llm = llm_with(hints=[hint("check your code")], verdicts=[verdict(False, ["not specific"])])

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)), max_hint=2)

    assert final["error_code"] == ErrorCode.hint_rejected
    assert "not specific" in final["error_message"]
    assert len(final["hint_attempts"]) == 2


# --------------------------------------------------------------------------- #
# Tool calling
# --------------------------------------------------------------------------- #


async def test_agent_calls_a_tool_and_the_result_reaches_the_hint():
    llm = llm_with(
        turns=[
            tool_turn("search_learning_materials", query="binary search off-by-one"),
            AIMessage("ready"),
        ],
        hints=[hint("mind the boundaries", ["algo-binary-search"])],
    )

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    tool_results = [m for m in final["research"] if isinstance(m, ToolMessage)]
    assert len(tool_results) == 1
    assert "algo-binary-search" in tool_results[0].content
    assert [ref.id for ref in final["materials"]] == ["algo-binary-search"]
    # the writing call sees the tool exchange, not just the opening prompt
    history = next(c for c in llm.calls if c.schema is HintResult).history
    assert any(isinstance(m, ToolMessage) for m in history)


async def test_tools_are_bound_to_the_hint_model():
    llm = llm_with()

    await run(llm, runner_returning(report(passed=False), report(passed=True)))

    bound = {t.name for t in llm.chat_calls[0].tools}
    assert {"search_learning_materials", "get_learning_material"} <= bound


async def test_made_up_material_ids_are_dropped():
    llm = llm_with(hints=[hint("read this", ["algo-does-not-exist"])])

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert final["materials"] == []


async def test_tool_loop_is_capped():
    llm = llm_with(turns=[tool_turn("list_material_topics")])

    final = await run(llm, runner_returning(report(passed=False), report(passed=True)))

    assert final["outcome"] == Outcome.hint_ready
    assert len(llm.chat_calls) == MAX_TOOL_ROUNDS


# --------------------------------------------------------------------------- #
# Checkpointing
# --------------------------------------------------------------------------- #


async def test_run_resumes_from_the_checkpoint_after_a_crash():
    llm = llm_with()
    calls = {"n": 0}

    async def flaky(code: str):
        calls["n"] += 1
        if calls["n"] == 1:
            return report(passed=False)
        if calls["n"] == 2:
            raise RuntimeError("sandbox died")
        return report(passed=True)

    saver = InMemorySaver()
    graph = build_graph(
        llm=llm,
        runner=flaky,
        fixer_model="fixer",
        hint_model="cheap",
        validator_model="strong",
        checkpointer=saver,
    )
    config = {"configurable": {"thread_id": "resumable"}}
    state = initial_state(
        task="sum of two numbers",
        student_code=BROKEN,
        tests=sample_tests(),
        max_fix_attempts=3,
        max_hint_attempts=3,
    )

    with pytest.raises(RuntimeError, match="sandbox died"):
        await graph.ainvoke(state, config)

    final = await graph.ainvoke(None, config)

    assert final["outcome"] == Outcome.hint_ready
    # the repair was checkpointed, so resuming did not pay the fixer a second time
    assert len([c for c in llm.calls if c.schema is FixResult]) == 1
