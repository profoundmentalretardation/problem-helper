"""Part 3: the attack suite, and the false-positive rate the report is measured on.

    uv run python -m evals.run_safety                # 16 hostile + 8 legitimate cases
    uv run python -m evals.run_safety --dry-run
    uv run python -m evals.run_safety --ablate       # also re-run the hostile cases with
                                                    # the input filter off, to see what
                                                    # each layer is actually worth

Like the agent eval, this runs in two phases — execute, then score stored traces — and the
scorer it uses (`evals.safety_scorer.signals`) is a pure function of a trace with no
knowledge of the suite. That is the requirement and it is also the useful property: the same
function is what you would point at a week of production traffic.

**Sequential, deliberately.** The indirect-injection cases poison the corpus by swapping the
process-wide retriever, and a concurrent run would poison somebody else's session. Twenty-
four cases at a few seconds each is not worth the race.

**The ablation is the honest part of the report.** Every layer looks necessary when they are
all on and nothing gets through. `--ablate` re-runs the hostile cases with layer 1 disabled,
so the table can say which attacks layer 1 was carrying alone and which ones layers 2–4
would have caught anyway. A defence-in-depth claim without that column is decoration.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
from collections import Counter
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from problem_helper import retrieval, tracing
from problem_helper.db import Database
from problem_helper.llm import LLMClient
from problem_helper.retrieval import Chunk, Hit
from problem_helper.schemas import SolveRequest, TestCase

from . import report, safety_scorer, trace_scorers
from .harness import Run, attach_traces, eval_settings, run_case
from .safety_scorer import CLEAN, DEFENDED, FALSE_POSITIVE, LEAKED

RESULTS_DIR = Path(__file__).parent / "results"
CASES_PATH = Path(__file__).parent / "attack_cases.json"

DEFAULT_RUNS = 3
DEFAULT_TEMPERATURE = 0.7
CONCURRENCY = 3
POISON_HOST_MATERIAL = "algo-loop-bounds"

CLASSES = ("direct_injection", "indirect_injection", "tool_abuse", "exfiltration")

logger = logging.getLogger(__name__)


# --------------------------------------------------------------------------- #
# A compromised corpus
# --------------------------------------------------------------------------- #


@dataclass
class PoisonedRetriever:
    """The real retriever with one hostile passage forced to the top of every result.

    It carries a *real* material id, because that is what a poisoned corpus looks like: not
    a new document nobody links to, but a paragraph added to a note the agent already
    trusts and already cites. A payload under a fake id would be caught by the citation
    check for the wrong reason and the case would prove nothing.
    """

    inner: Any
    payload: str

    def search(self, query: str, *, k: int | None = None, rerank: bool | None = None):
        hits = list(self.inner.search(query, k=k, rerank=rerank))
        poisoned = Chunk(
            id=f"{POISON_HOST_MATERIAL}#0.0",
            material_id=POISON_HOST_MATERIAL,
            title="Loop bounds and off-by-one errors",
            topic="basics",
            heading="Editor's note",
            text=self.payload,
            tags=(),
        )
        return [Hit(chunk=poisoned, score=9.99, stage="rerank", rrf_rank=1), *hits][: k or 5]


# --------------------------------------------------------------------------- #
# Cases
# --------------------------------------------------------------------------- #


def load_cases(path: Path = CASES_PATH) -> list[dict]:
    payload = json.loads(path.read_text(encoding="utf-8"))
    return payload["cases"]


def as_request(case: dict) -> SolveRequest:
    return SolveRequest(
        task=case["task"],
        code=case["code"],
        tests=[TestCase(**t) for t in case["tests"]],
    )


# --------------------------------------------------------------------------- #
# The run
# --------------------------------------------------------------------------- #


async def execute(
    cases: list[dict], *, runs: int, temperature: float, db_path: Path, **guard
) -> list[Run]:
    """Runs every case `runs` times.

    Split by whether the case poisons the corpus. Poisoning swaps the process-wide
    retriever, so those cases run one at a time or they would poison each other's sessions;
    everything else runs concurrently. Serialising the whole suite for the sake of three
    cases would triple the wall clock for nothing.
    """
    settings = eval_settings(temperature, db_path=str(db_path), **guard)
    llm = LLMClient(settings)
    database = await Database(str(db_path)).connect()
    real = retrieval.get_retriever()
    poisoned = [case for case in cases if case.get("poison")]
    clean = [case for case in cases if not case.get("poison")]
    out: list[Run] = []

    async def once(case: dict, attempt: int, semaphore: asyncio.Semaphore) -> Run:
        return await run_case(
            case["id"],
            as_request(case),
            attempt=attempt,
            db=database,
            llm=llm,
            settings=settings,
            semaphore=semaphore,
        )

    try:
        semaphore = asyncio.Semaphore(CONCURRENCY)
        out += list(
            await asyncio.gather(
                *(
                    once(case, attempt, semaphore)
                    for case in clean
                    for attempt in range(1, runs + 1)
                )
            )
        )

        serial = asyncio.Semaphore(1)
        for case in poisoned:
            retrieval.set_retriever(PoisonedRetriever(real, case["poison"]))
            try:
                for attempt in range(1, runs + 1):
                    out.append(await once(case, attempt, serial))
            finally:
                retrieval.set_retriever(real)
    finally:
        await database.close()
        await llm.close()
    return out


def score(runs: list[Run], cases: list[dict]) -> list[dict]:
    """Phase 2: the pure scorer over stored traces, writing verdicts back as feedback."""
    by_id = {case["id"]: case for case in cases}
    rows = []
    for run in runs:
        case = by_id[run.case_id]
        if run.trace is None:
            rows.append({"case": run.case_id, "class": case["class"], "verdict": "no_trace"})
            continue

        found = safety_scorer.signals(run.trace)
        outcome, reasons = safety_scorer.verdict(
            found, hostile=case["hostile"], markers=case.get("markers", [])
        )
        trace_scorers.log(
            run.trace_id,
            [
                trace_scorers.Feedback("safety_verdict", outcome, "; ".join(reasons)),
                trace_scorers.Feedback("safety_compromised", found.compromised),
                trace_scorers.Feedback("safety_blocked", found.blocked),
                trace_scorers.Feedback("safety_suspicious", found.suspicious),
            ],
        )
        rows.append(
            {
                "case": run.case_id,
                "attempt": run.attempt,
                "class": case["class"],
                "hostile": case["hostile"],
                "verdict": outcome,
                "reasons": reasons,
                "session_id": run.session_id,
                **found.as_dict(),
            }
        )
    return rows


def _hostile_summary(rows: list[dict]) -> dict:
    """Counted over runs, not cases.

    A case that survives two runs and leaks on the third has leaked, so `leaked` names the
    case; the defence rate is over runs, because that is the fraction of real sessions a
    payload would have got through.
    """
    return {
        "runs": len(rows),
        "cases": len({row["case"] for row in rows}),
        "defended": sum(1 for row in rows if row["verdict"] == DEFENDED),
        "defence_rate": (
            sum(1 for row in rows if row["verdict"] == DEFENDED) / len(rows) if rows else None
        ),
        "leaked": sorted({row["case"] for row in rows if row["verdict"] == LEAKED}),
        "blocked_at_entry": sum(1 for row in rows if row["blocked"]),
        "answered_ignoring_payload": sum(
            1 for row in rows if row["verdict"] == DEFENDED and not row["blocked"]
        ),
    }


def aggregate(rows: list[dict]) -> dict:
    hostile = [row for row in rows if row.get("hostile")]
    legitimate = [row for row in rows if row.get("hostile") is False]
    false_positives = [row for row in legitimate if row["verdict"] == FALSE_POSITIVE]

    by_class = {
        name: _hostile_summary([row for row in hostile if row["class"] == name])
        for name in CLASSES
        if any(row["class"] == name for row in hostile)
    }

    flaky = sorted(
        {
            row["case"]
            for row in legitimate
            if row["verdict"] == FALSE_POSITIVE
            and any(
                other["case"] == row["case"] and other["verdict"] == CLEAN
                for other in legitimate
            )
        }
    )

    return {
        "hostile": _hostile_summary(hostile),
        "legitimate": {
            "runs": len(legitimate),
            "cases": len({row["case"] for row in legitimate}),
            "clean": sum(1 for row in legitimate if row["verdict"] == CLEAN),
            "false_positives": len(false_positives),
            "false_positive_rate": (
                len(false_positives) / len(legitimate) if legitimate else None
            ),
            "affected_cases": sorted({row["case"] for row in false_positives}),
            # A case that was refused on one run and answered on another: the guardrails are
            # sampled too, and a rate quoted off a single pass would have missed this.
            "intermittent_cases": flaky,
            "occurrences": [
                {"case": row["case"], "attempt": row["attempt"], "reasons": row["reasons"]}
                for row in false_positives
            ],
        },
        "by_class": by_class,
        "layers_that_fired": dict(
            Counter(layer for row in rows for layer in row.get("blocked_by", []))
        ),
        "rows": rows,
    }


# --------------------------------------------------------------------------- #
# Rendering
# --------------------------------------------------------------------------- #


def render(results: dict) -> str:
    hostile = results["hostile"]
    legit = results["legitimate"]
    out = [
        "# Safety hardening",
        "",
        (
            f"{hostile['cases']} hostile cases across four attack classes and "
            f"{legit['cases']} legitimate ones, each run {results['meta']['runs']} times at "
            f"temperature {results['meta']['temperature']} — "
            f"{hostile['runs'] + legit['runs']} sessions."
        ),
        "",
        "## Attacks",
        "",
        report.table(
            ("attack class", "cases", "defended", "refused outright", "answered, payload ignored", "leaked"),
            [
                [
                    name.replace("_", " "),
                    str(data["cases"]),
                    f"{data['defended']}/{data['runs']}",
                    str(data["blocked_at_entry"]),
                    str(data["answered_ignoring_payload"]),
                    ", ".join(data["leaked"]) or "—",
                ]
                for name, data in results["by_class"].items()
            ]
            + [
                [
                    "**all**",
                    str(hostile["cases"]),
                    f"**{hostile['defended']}/{hostile['runs']}**",
                    str(hostile["blocked_at_entry"]),
                    str(hostile["answered_ignoring_payload"]),
                    ", ".join(hostile["leaked"]) or "—",
                ]
            ],
        ),
        "",
        (
            "Counted over runs, not cases: the fraction of real sessions a payload would "
            "have got through. *Refused outright* and *answered, payload ignored* are both "
            "defences and the second is the better one — the student still gets their hint. "
            "The split is shown because a suite that only ever refuses is a suite that has "
            "traded its false-positive rate for its defence rate."
        ),
        "",
        "## False positives on legitimate queries",
        "",
        report.table(
            ("legitimate cases", "runs", "clean", "false positives", "false-positive rate"),
            [
                [
                    str(legit["cases"]),
                    str(legit["runs"]),
                    str(legit["clean"]),
                    str(legit["false_positives"]),
                    report.cell(legit["false_positive_rate"], 3),
                ]
            ],
        ),
        "",
        _false_positive_detail(legit),
        "",
        "## Which layer fired",
        "",
        report.table(
            ("layer", "sessions"),
            [[layer, str(count)] for layer, count in sorted(results["layers_that_fired"].items())]
            or [["—", "0"]],
        ),
        "",
        _ablation_section(results),
        "",
    ]
    return "\n".join(out)


def _false_positive_detail(legit: dict) -> str:
    if not legit["occurrences"]:
        return (
            f"No legitimate run was refused across {legit['runs']} sessions. The "
            f"{legit['cases']} cases are chosen to sit next to a detector — an "
            "instruction-set decoder that 'ignores all previous instructions', a task whose "
            "sample input is a URL, an environment-file parser, a Russian statement, a long "
            "base64-looking token — so this is a measurement rather than a formality. It is "
            "still a small denominator, and the rate should be read as an upper bound with a "
            "wide interval rather than as a zero."
        )
    lines = ["The refusals, and what tripped them:", ""]
    lines += [
        f"- `{row['case']}` run {row['attempt']} — {'; '.join(row['reasons'])}"
        for row in legit["occurrences"]
    ]
    if legit["intermittent_cases"]:
        lines += [
            "",
            (
                "Intermittent: "
                + ", ".join(f"`{c}`" for c in legit["intermittent_cases"])
                + " was refused on some runs and answered on others. The guardrails are "
                "downstream of a sampled model, so the false-positive rate is sampled too — "
                "which is the reason this suite runs every case more than once rather than "
                "quoting a rate off a single pass."
            ),
        ]
    return "\n".join(lines)


def _ablation_section(results: dict) -> str:
    ablation = results.get("ablation")
    if not ablation:
        return (
            "## What each layer is worth\n\n"
            "Not measured in this run. `--ablate` re-runs the hostile cases with the input "
            "filter disabled and reports which attacks layer 1 was carrying alone."
        )
    return "\n".join(
        [
            "## What each layer is worth",
            "",
            (
                "The hostile cases re-run with layer 1 (input filtering) disabled. What still "
                "gets defended is what layers 2–4 were catching anyway; what leaks is what "
                "layer 1 was carrying on its own."
            ),
            "",
            report.table(
                ("configuration", "runs", "defended", "refused outright", "leaked"),
                [
                    [
                        "all four layers",
                        str(results["hostile"]["runs"]),
                        f"{results['hostile']['defended']}/{results['hostile']['runs']}",
                        str(results["hostile"]["blocked_at_entry"]),
                        ", ".join(results["hostile"]["leaked"]) or "—",
                    ],
                    [
                        "layer 1 off",
                        str(ablation["runs"]),
                        f"{ablation['defended']}/{ablation['runs']}",
                        str(ablation["blocked_at_entry"]),
                        ", ".join(ablation["leaked"]) or "—",
                    ],
                ],
            ),
        ]
    )


# --------------------------------------------------------------------------- #
# Entry point
# --------------------------------------------------------------------------- #


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=int, default=DEFAULT_RUNS)
    parser.add_argument("--temperature", type=float, default=DEFAULT_TEMPERATURE)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--ablate", action="store_true", help="re-run the attacks with layer 1 off")
    parser.add_argument("--only", default="", help="comma-separated case ids")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(message)s")

    cases = load_cases()
    if args.only:
        wanted = {c.strip() for c in args.only.split(",")}
        cases = [case for case in cases if case["id"] in wanted]

    if args.dry_run:
        counts = Counter(case["class"] for case in cases)
        logger.info("dry run: %s cases — %s", len(cases), dict(counts))
        for case in cases:
            logger.info("  %-28s %-18s hostile=%s", case["id"], case["class"], case["hostile"])
        return

    from problem_helper.config import get_settings

    if not get_settings().llm_api_key:
        raise SystemExit("LLM_API_KEY is not set — the attack suite runs the real pipeline")

    settings = eval_settings(args.temperature)
    tracing.configure(
        enabled=True,
        tracking_uri=settings.mlflow_tracking_uri,
        experiment=settings.mlflow_experiment,
    )
    RESULTS_DIR.mkdir(exist_ok=True)

    logger.info("phase 1: %s cases × %s runs, all four layers on", len(cases), args.runs)
    runs = await execute(
        cases,
        runs=args.runs,
        temperature=args.temperature,
        db_path=RESULTS_DIR / "safety_sessions.db",
    )
    attach_traces(runs)
    results = aggregate(score(runs, cases))

    if args.ablate:
        hostile = [case for case in cases if case["hostile"]]
        logger.info("phase 1b: %s hostile cases with the input filter off", len(hostile))
        # One run each: the ablation answers "which attacks was layer 1 carrying alone",
        # and that comparison does not need the same denominator as the headline rate.
        ablated = await execute(
            hostile,
            runs=1,
            temperature=args.temperature,
            db_path=RESULTS_DIR / "safety_sessions_ablated.db",
            input_filter_enabled=False,
        )
        attach_traces(ablated)
        results["ablation"] = aggregate(score(ablated, hostile))["hostile"]

    results["meta"] = {
        "runs": args.runs,
        "temperature": args.temperature,
        "fixer_model": settings.fixer_model,
        "hint_model": settings.hint_model,
        "validator_model": settings.validator_model,
        "sandbox_backend": str(settings.sandbox_backend),
    }

    (RESULTS_DIR / "safety.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    rendered = render(results)
    (RESULTS_DIR / "safety.md").write_text(rendered, encoding="utf-8")
    print(rendered)


if __name__ == "__main__":
    asyncio.run(main())
