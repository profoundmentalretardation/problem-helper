"""Part 1: the agent evaluation suite.

    uv run python -m evals.run_agent               # 13 scenarios × 3 runs
    uv run python -m evals.run_agent --dry-run     # what it would run, no calls
    uv run python -m evals.run_agent --runs 5      # more runs per scenario
    uv run python -m evals.run_agent --no-judge    # trajectory metrics only, no judge calls

The run has two phases on purpose, and the split is the point of having done Part 2 first:

1. **Execute.** Every scenario runs `--runs` times through the real orchestrator, at a
   temperature above zero, with `request_origin=batch` and `eval_case_id` on the trace.
   Nothing is measured here.
2. **Score.** Every trace is looked up by its session tag and handed to the scorers in
   `evals.trace_scorers`, which write their verdicts back with `mlflow.log_feedback`.

Phase 2 never touches the pipeline, so it also runs against traces from yesterday, from
production, or from a colleague's machine. If the metrics had been computed inline, the
suite would be a test harness; computed off the trace, it is a monitor that happens to have
an eval set attached.

**Temperature.** `--temperature`, default 0.7. Requirement 6 asks which scenario varies
between runs, and at the course-standard 0.0 the answer would be "none, by construction".
The service itself still runs at 0.0.

**pass@1 / pass@k / pass^k.** All three are reported. At k=3, pass@k is "did it ever pass"
and is nearly free; pass^k is the one that describes an agent you could put in front of
students, and it is the column the README leads with.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
from collections import Counter
from pathlib import Path

from problem_helper import samples, tracing
from problem_helper.db import Database
from problem_helper.llm import LLMClient
from problem_helper.schemas import SolveRequest, TestCase

from . import agent_metrics, report, trace_scorers
from .agent_metrics import Expectation
from .harness import Run, attach_traces, eval_settings, run_case
from .judge import Judge, ResponseCache

RESULTS_DIR = Path(__file__).parent / "results"
CASES_PATH = Path(__file__).parent / "agent_cases.json"

DEFAULT_RUNS = 3
DEFAULT_TEMPERATURE = 0.7
CONCURRENCY = 3

JUDGE_MODEL = os.getenv("EVAL_JUDGE_MODEL", "anthropic/claude-haiku-4.5")

logger = logging.getLogger(__name__)


# --------------------------------------------------------------------------- #
# Cases
# --------------------------------------------------------------------------- #


def load_cases(path: Path = CASES_PATH) -> list[dict]:
    """Reads the case file and resolves every `sample` anchor against the live catalog."""
    payload = json.loads(path.read_text(encoding="utf-8"))
    cases = []
    for case in payload["cases"]:
        cases.append({**case, **_body(case)})
    return cases


def _body(case: dict) -> dict:
    """The task, code and tests — from the samples catalog, or inline for the extra cases."""
    if "sample" not in case:
        return {
            "task": case["task"],
            "code": case["code"],
            "tests": [TestCase(**t) for t in case["tests"]],
        }
    sample = next((s for s in samples.all() if s.id == case["sample"]), None)
    if sample is None:
        raise ValueError(f"case {case['id']!r} anchors on unknown sample {case['sample']!r}")
    return {"task": sample.task, "code": sample.code, "tests": list(sample.tests)}


def as_request(case: dict) -> SolveRequest:
    return SolveRequest(task=case["task"], code=case["code"], tests=case["tests"])


# --------------------------------------------------------------------------- #
# The run
# --------------------------------------------------------------------------- #


async def execute(cases: list[dict], *, runs: int, temperature: float, db_path: Path) -> list[Run]:
    _require_key()
    settings = eval_settings(temperature, db_path=str(db_path))
    llm = LLMClient(settings)
    database = await Database(str(db_path)).connect()
    semaphore = asyncio.Semaphore(CONCURRENCY)
    try:
        pending = [
            run_case(
                case["id"],
                as_request(case),
                attempt=attempt,
                db=database,
                llm=llm,
                settings=settings,
                semaphore=semaphore,
            )
            for case in cases
            for attempt in range(1, runs + 1)
        ]
        return list(await asyncio.gather(*pending))
    finally:
        await database.close()
        await llm.close()


async def score(
    runs: list[Run], cases: list[dict], *, judge: Judge | None
) -> dict[str, list[dict]]:
    """Phase 2: the scorers, over stored traces, writing back with `log_feedback`."""
    expectations = {case["id"]: Expectation.from_case(case) for case in cases}
    scored: dict[str, list[dict]] = {case["id"]: [] for case in cases}

    for run in runs:
        if run.trace is None:
            scored[run.case_id].append({"attempt": run.attempt, "missing_trace": True})
            continue

        feedbacks, detail = trace_scorers.score_trajectory(
            run.trace, expectations[run.case_id]
        )
        if judge is not None:
            rag_feedbacks, rag_detail = await trace_scorers.score_rag(run.trace, judge)
            feedbacks += rag_feedbacks
            detail["rag"] = rag_detail
            detail |= {f.name: f.value for f in rag_feedbacks}

        detail["passed"] = trace_scorers.passed(detail)
        feedbacks.append(
            trace_scorers.Feedback(
                "scenario_passed",
                detail["passed"],
                (
                    "tool selection and goal completion both 1.0"
                    if detail["passed"]
                    else "; ".join(detail["completion_reasons"])
                    or "tool selection did not match"
                ),
            )
        )
        detail["feedback_written"] = trace_scorers.log(run.trace_id, feedbacks)
        detail["attempt"] = run.attempt
        detail["trace_id"] = run.trace_id
        detail["session_id"] = run.session_id
        scored[run.case_id].append(detail)

    return scored


def aggregate(scored: dict[str, list[dict]], cases: list[dict]) -> dict:
    by_case = {}
    for case in cases:
        rows = sorted(scored[case["id"]], key=lambda r: r["attempt"])
        passes = [bool(row.get("passed")) for row in rows]
        by_case[case["id"]] = {
            "category": case["category"],
            "runs": rows,
            "passed": passes,
            "flaky": agent_metrics.flaky(passes),
            **agent_metrics.pass_rates(passes),
            **agent_metrics.summarise(rows, trace_scorers.TRAJECTORY_METRICS),
        }

    every_row = [row for rows in scored.values() for row in rows]
    rag_rows = [row for row in every_row if row.get("rag_faithfulness") is not None]
    return {
        "by_case": by_case,
        "overall": {
            **agent_metrics.summarise(every_row, trace_scorers.TRAJECTORY_METRICS),
            "pass@1": _mean([c["pass@1"] for c in by_case.values()]),
            "pass@k": _mean([c["pass@k"] for c in by_case.values()]),
            "pass^k": _mean([c["pass^k"] for c in by_case.values()]),
            "flaky_cases": [cid for cid, c in by_case.items() if c["flaky"]],
            "runs_scored": len(every_row),
            "runs_missing_trace": sum(1 for row in every_row if row.get("missing_trace")),
        },
        "rag": {
            **agent_metrics.summarise(
                rag_rows, tuple(f"rag_{m}" for m in trace_scorers.RAG_METRICS[:3])
            ),
            "runs_judged": len(rag_rows),
            "runs_without_retrieval": len(every_row) - len(rag_rows),
        },
        "by_category": _by_category(by_case),
    }


def _by_category(by_case: dict) -> dict:
    grouped: dict[str, list[dict]] = {}
    for data in by_case.values():
        grouped.setdefault(data["category"], []).append(data)
    return {
        category: {
            "n": len(entries),
            "pass@1": _mean([e["pass@1"] for e in entries]),
            "pass^k": _mean([e["pass^k"] for e in entries]),
        }
        for category, entries in sorted(grouped.items())
    }


def _mean(values: list[float]) -> float | None:
    return sum(values) / len(values) if values else None


# --------------------------------------------------------------------------- #
# Rendering
# --------------------------------------------------------------------------- #


def render(results: dict) -> str:
    meta = results["meta"]
    overall = results["overall"]
    out = [
        "# Agent evaluation",
        "",
        (
            f"{meta['cases']} scenarios × {meta['runs']} runs at temperature "
            f"{meta['temperature']}, models: fixer `{meta['fixer_model']}`, hint "
            f"`{meta['hint_model']}`, validator `{meta['validator_model']}`."
        ),
        "",
        "## Per scenario",
        "",
        report.table(
            ("scenario", "category", "runs", "pass@1", "pass@3", "pass^3", "tool sel.",
             "tool params", "traj. P", "traj. R", "goal"),
            [
                [
                    case_id,
                    data["category"],
                    "".join("✓" if p else "✗" for p in data["passed"]),
                    report.cell(data["pass@1"], 2),
                    report.cell(data["pass@k"], 2),
                    report.cell(data["pass^k"], 2),
                    report.cell(data["tool_selection_accuracy"], 2),
                    report.cell(data["tool_parameter_accuracy"], 2),
                    report.cell(data["trajectory_precision"], 2),
                    report.cell(data["trajectory_recall"], 2),
                    report.cell(data["goal_completion"], 2),
                ]
                for case_id, data in results["by_case"].items()
            ],
        ),
        "",
        report.table(
            ("overall", "pass@1", "pass@3", "pass^3", "tool sel.", "tool params",
             "traj. P", "traj. R", "goal"),
            [
                [
                    f"{meta['cases']} scenarios",
                    report.cell(overall["pass@1"], 2),
                    report.cell(overall["pass@k"], 2),
                    report.cell(overall["pass^k"], 2),
                    report.cell(overall["tool_selection_accuracy"], 2),
                    report.cell(overall["tool_parameter_accuracy"], 2),
                    report.cell(overall["trajectory_precision"], 2),
                    report.cell(overall["trajectory_recall"], 2),
                    report.cell(overall["goal_completion"], 2),
                ]
            ],
        ),
        "",
        "## By category",
        "",
        report.table(
            ("category", "n", "pass@1", "pass^3"),
            [
                [category, str(data["n"]), report.cell(data["pass@1"], 2),
                 report.cell(data["pass^k"], 2)]
                for category, data in results["by_category"].items()
            ],
        ),
        "",
        "## Variance",
        "",
        _variance_section(results),
        "",
        "## Generation metrics over the same traces",
        "",
        _rag_section(results),
        "",
    ]
    return "\n".join(out)


def _variance_section(results: dict) -> str:
    flaky = results["overall"]["flaky_cases"]
    if not flaky:
        return (
            "No scenario split its runs. Every case was 3-for-3 or 0-for-3, which usually "
            "means the tolerances are too loose or the scenarios too easy rather than that "
            "the agent is perfectly stable — read the table above with that in mind."
        )
    named = ", ".join(f"`{case_id}`" for case_id in flaky)
    lines = [f"{len(flaky)} scenario(s) passed on some runs and failed on others: {named}.", ""]
    for case_id in flaky:
        data = results["by_case"][case_id]
        lines.append(f"**`{case_id}`** — {_what_varied(data)}")
        lines.append("")
    return "\n".join(lines)


def _what_varied(data: dict) -> str:
    """The difference between the passing and failing runs of one scenario, in words."""
    trails = [
        " → ".join(step["tool"] for step in row.get("trajectory", [])) or "<no tool call>"
        for row in data["runs"]
    ]
    reasons = [
        "; ".join(row.get("completion_reasons") or []) for row in data["runs"]
    ]
    shapes = Counter(trails)
    parts = [
        "trajectories across runs: "
        + ", ".join(f"`{trail}` ×{count}" for trail, count in shapes.items())
    ]
    failing = [
        f"run {row['attempt']}: {reason}"
        for row, reason in zip(data["runs"], reasons, strict=False)
        if not row.get("passed") and reason
    ]
    if failing:
        parts.append("failing runs reported " + "; ".join(failing))
    else:
        parts.append(
            "the failing run(s) reached the expected outcome but took a trajectory outside "
            "the acceptable set"
        )
    return ". ".join(parts) + "."


def _rag_section(results: dict) -> str:
    rag = results["rag"]
    if not rag["runs_judged"]:
        return "Not run (`--no-judge`), or no run in this batch retrieved anything."
    return "\n".join(
        [
            (
                f"The HW2 scorers, unchanged, over the {rag['runs_judged']} traced runs where "
                f"the agent actually retrieved. {rag['runs_without_retrieval']} run(s) never "
                "called the corpus and are not averaged in: faithfulness against an empty "
                "context set is undefined, not zero."
            ),
            "",
            report.table(
                ("faithfulness", "answer relevance", "context precision"),
                [
                    [
                        report.cell(rag.get("rag_faithfulness")),
                        report.cell(rag.get("rag_answer_relevance")),
                        report.cell(rag.get("rag_context_precision")),
                    ]
                ],
            ),
            "",
            (
                "Context recall is absent by construction: it scores a context set against a "
                "*reference answer*, and an agent trace has no golden hint to compare with. "
                "It stays in the retrieval harness, where the eval set supplies one."
            ),
        ]
    )


# --------------------------------------------------------------------------- #
# Entry point
# --------------------------------------------------------------------------- #


def _require_key() -> None:
    from problem_helper.config import get_settings

    if not get_settings().llm_api_key:
        raise SystemExit("LLM_API_KEY is not set — the agent eval runs the real pipeline")


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=int, default=DEFAULT_RUNS)
    parser.add_argument("--temperature", type=float, default=DEFAULT_TEMPERATURE)
    parser.add_argument("--dry-run", action="store_true", help="plan the run, make no calls")
    parser.add_argument("--no-judge", action="store_true", help="skip the HW2 scorers")
    parser.add_argument("--only", default="", help="comma-separated case ids")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(message)s")

    cases = load_cases()
    if args.only:
        wanted = {c.strip() for c in args.only.split(",")}
        cases = [case for case in cases if case["id"] in wanted]
        if not cases:
            raise SystemExit(f"no case matches {sorted(wanted)}")

    if args.dry_run:
        logger.info(
            "dry run: %s scenarios × %s runs = %s pipeline executions at temperature %s",
            len(cases),
            args.runs,
            len(cases) * args.runs,
            args.temperature,
        )
        for case in cases:
            logger.info("  %-24s %-14s %s", case["id"], case["category"], case["expected_tools"])
        return

    settings = eval_settings(args.temperature)
    tracing.configure(
        enabled=True,
        tracking_uri=settings.mlflow_tracking_uri,
        experiment=settings.mlflow_experiment,
    )
    RESULTS_DIR.mkdir(exist_ok=True)

    logger.info("phase 1: %s scenarios × %s runs", len(cases), args.runs)
    runs = await execute(
        cases,
        runs=args.runs,
        temperature=args.temperature,
        db_path=RESULTS_DIR / "agent_sessions.db",
    )

    logger.info("phase 2: scoring %s traced runs", len(runs))
    found = attach_traces(runs)
    logger.info("  %s/%s runs had a trace", found, len(runs))

    cache = ResponseCache()
    judge_llm = None if args.no_judge else LLMClient(eval_settings(0.0))
    judge = None if judge_llm is None else Judge(judge_llm, model=JUDGE_MODEL, cache=cache)
    try:
        scored = await score(runs, cases, judge=judge)
    finally:
        if judge_llm is not None:
            cache.save()
            await judge_llm.close()

    results = aggregate(scored, cases)
    results["meta"] = {
        "cases": len(cases),
        "runs": args.runs,
        "temperature": args.temperature,
        "fixer_model": settings.fixer_model,
        "hint_model": settings.hint_model,
        "validator_model": settings.validator_model,
        "judge_model": None if args.no_judge else JUDGE_MODEL,
        "sandbox_backend": str(settings.sandbox_backend),
    }

    (RESULTS_DIR / "agent.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    rendered = render(results)
    (RESULTS_DIR / "agent.md").write_text(rendered, encoding="utf-8")
    print(rendered)


if __name__ == "__main__":
    asyncio.run(main())
