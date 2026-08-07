"""Part 3: the judged generation metrics, with and without reranking.

    uv run python -m evals.run_generation            # both configurations
    uv run python -m evals.run_generation --dry-run  # what it would cost, no calls

Every model response — the answers under test as well as the judgements — is cached on disk
(`results/response_cache.json`), so a second run costs nothing and reproduces byte for byte.
Caching the judgements alone would not be enough: the answering model is sampled, and a new
answer is a new judge prompt. Delete the cache to re-run for real.

**Which subset the reranking-off run uses, and why.** Reranking only reorders — and at
`top_k` sometimes replaces — the passages fusion produced. When the retrieved context is
*identical* between the two configurations, both runs feed the judge the same question, the
same context and (via the cache) the same answer, so any difference in the score would be
sampling noise rather than a measurement of the reranker. The no-rerank run therefore covers
exactly the cases whose context actually changed, plus every out-of-corpus case, where the
question is whether the answer refuses rather than what was retrieved. The unchanged cases
are reported with their reranked scores and counted, not silently dropped.

**Judge model ≠ answering model.** Configured through `EVAL_ANSWER_MODEL` and
`EVAL_JUDGE_MODEL`; the runner refuses to start when they are equal, because a model
grading its own output is the one bias no prompt wording fixes.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
from pathlib import Path

from problem_helper.config import get_settings
from problem_helper.llm import LLMClient
from problem_helper.retrieval import RetrievalService

from . import report
from .dataset import EvalCase, load
from .generation import answer_question, retrieve_contexts
from .judge import Judge, ResponseCache

RESULTS_DIR = Path(__file__).parent / "results"
TOP_K = 5
CONCURRENCY = 4

ANSWER_MODEL = os.getenv("EVAL_ANSWER_MODEL", "google/gemini-3.5-flash-lite")
JUDGE_MODEL = os.getenv("EVAL_JUDGE_MODEL", "anthropic/claude-haiku-4.5")

METRICS = ("faithfulness", "answer_relevance", "context_precision", "context_recall")

logger = logging.getLogger(__name__)


async def score_one(
    case: EvalCase,
    *,
    contexts: list[str],
    chunk_ids: list[str],
    llm,
    judge: Judge,
    semaphore: asyncio.Semaphore,
    attempts: int = 4,
) -> dict:
    """Answer and judge one case, retrying the whole case on a provider hiccup.

    A judged run is hundreds of calls long and an aggregator will drop one of them sooner or
    later (a 200 with an empty body, a rate limit). Everything already judged is in the
    cache, so a retry re-does only the call that failed.
    """
    for attempt in range(1, attempts + 1):
        try:
            return await _score_once(
                case,
                contexts=contexts,
                chunk_ids=chunk_ids,
                llm=llm,
                judge=judge,
                semaphore=semaphore,
            )
        except Exception as exc:
            if attempt == attempts:
                raise
            logger.warning(
                "  %-32s attempt %s/%s failed (%s), retrying", case.id, attempt, attempts, exc
            )
            await asyncio.sleep(2 * attempt)
    raise AssertionError("unreachable")


async def _score_once(
    case: EvalCase,
    *,
    contexts: list[str],
    chunk_ids: list[str],
    llm,
    judge: Judge,
    semaphore: asyncio.Semaphore,
) -> dict:
    async with semaphore:
        answer = await answer_question(
            llm,
            model=ANSWER_MODEL,
            question=case.query,
            contexts=contexts,
            cache=judge.cache,
        )
        faithfulness = await judge.faithfulness(
            question=case.query, answer=answer.answer, contexts=contexts
        )
        relevance = await judge.answer_relevance(question=case.query, answer=answer.answer)
        precision = await judge.context_precision(question=case.query, contexts=contexts)
        recall = await judge.context_recall(
            golden_answer=case.golden_answer, contexts=contexts
        )
    logger.info(
        "  %-32s faith %.2f  rel %.2f  cprec %.2f  crec %.2f",
        case.id,
        faithfulness["score"],
        relevance["score"],
        precision["score"],
        recall["score"],
    )
    return {
        "case": case.id,
        "category": case.category,
        "chunk_ids": chunk_ids,
        "answer": answer.answer,
        "faithfulness": faithfulness,
        "answer_relevance": relevance,
        "context_precision": precision,
        "context_recall": recall,
    }


def summarise(rows: list[dict]) -> dict[str, float | None]:
    if not rows:
        return {metric: None for metric in METRICS}
    return {
        metric: sum(row[metric]["score"] for row in rows) / len(rows) for metric in METRICS
    }


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true", help="plan the run, make no calls")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(message)s")

    if ANSWER_MODEL == JUDGE_MODEL:
        raise SystemExit(
            f"refusing to run: the judge and the answering model are both {JUDGE_MODEL!r}. "
            "Set EVAL_JUDGE_MODEL to a model from a different family."
        )

    evalset = load()
    service = RetrievalService(cache_dir=Path(".rag_cache"))

    # Retrieve both ways up front: the comparison decides which cases the cheap run covers.
    contexts = {
        case.id: {
            "rerank": retrieve_contexts(service, case.query, k=TOP_K, rerank=True),
            "no_rerank": retrieve_contexts(service, case.query, k=TOP_K, rerank=False),
        }
        for case in evalset.cases
    }
    def context_changed(case: EvalCase) -> bool:
        """Set membership, not order: a reshuffle of the same five chunks is not new context."""
        return set(contexts[case.id]["rerank"][1]) != set(contexts[case.id]["no_rerank"][1])

    changed = [case for case in evalset.cases if context_changed(case) or case.unanswerable]
    logger.info(
        "%s cases; reranking replaced at least one chunk in %s of them, so the no-rerank run "
        "covers %s cases — a subset that saves %s case(s), which is why it is reported as "
        "near-complete rather than as a sample",
        len(evalset.cases),
        sum(context_changed(case) for case in evalset.cases),
        len(changed),
        len(evalset.cases) - len(changed),
    )

    plan = {"rerank": evalset.cases, "no_rerank": changed}
    if args.dry_run:
        calls = sum(len(cases) * (1 + 3 + TOP_K) for cases in plan.values())
        logger.info(
            "dry run: %s cases to answer and judge → at most %s model calls "
            "(cache hits cost nothing)",
            sum(len(c) for c in plan.values()),
            calls,
        )
        return

    settings = get_settings()
    if not settings.llm_api_key:
        raise SystemExit("LLM_API_KEY is not set — the judged metrics need a provider")
    llm = LLMClient(settings)
    cache = ResponseCache()
    judge = Judge(llm, model=JUDGE_MODEL, cache=cache)
    semaphore = asyncio.Semaphore(CONCURRENCY)

    results: dict = {
        "answer_model": ANSWER_MODEL,
        "judge_model": JUDGE_MODEL,
        "top_k": TOP_K,
        "runs": {},
    }
    try:
        for name, cases in plan.items():
            logger.info("run %s (%s cases)", name, len(cases))
            rows = await asyncio.gather(
                *(
                    score_one(
                        case,
                        contexts=contexts[case.id][name][0],
                        chunk_ids=contexts[case.id][name][1],
                        llm=llm,
                        judge=judge,
                        semaphore=semaphore,
                    )
                    for case in cases
                )
            )
            by_category: dict[str, dict] = {}
            for category in sorted({row["category"] for row in rows}):
                subset = [row for row in rows if row["category"] == category]
                by_category[category] = {"n": len(subset), **summarise(subset)}
            answerable = [row for row in rows if row["category"] != "out_of_corpus"]
            results["runs"][name] = {
                "cases": [case.id for case in cases],
                "summary": summarise(rows),
                # Out-of-corpus cases score 0 on relevance by construction — a correct
                # refusal does not address the question — so the mixed average understates
                # every configuration equally. Both are reported; neither is the headline.
                "summary_answerable": summarise(answerable),
                "answerable": len(answerable),
                "by_category": by_category,
                "per_case": rows,
            }
    finally:
        cache.save()
        await llm.close()

    results["cache"] = {"hits": cache.hits, "misses": cache.misses}
    logger.info("judge cache: %s hits, %s calls", cache.hits, cache.misses)

    RESULTS_DIR.mkdir(exist_ok=True)
    (RESULTS_DIR / "generation.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    rendered = render(results)
    (RESULTS_DIR / "generation.md").write_text(rendered, encoding="utf-8")
    print(rendered)


def render(results: dict) -> str:
    headers = ("run", "cases", "faithfulness", "answer relevance", "context precision", "context recall")

    def row(label: str, data: dict, key: str = "summary") -> list[str]:
        summary = data[key]
        count = len(data["cases"]) if key == "summary" else data["answerable"]
        return [label, str(count), *(report.cell(summary[m]) for m in METRICS)]

    out = [
        "# Generation metrics",
        "",
        (
            f"Answering model `{results['answer_model']}`, judge "
            f"`{results['judge_model']}`, top-{results['top_k']} passages."
        ),
        "",
        report.table(
            headers,
            [
                row("reranking on", results["runs"]["rerank"]),
                row("reranking off", results["runs"]["no_rerank"]),
                row("reranking on, answerable only", results["runs"]["rerank"], "summary_answerable"),
                row("reranking off, answerable only", results["runs"]["no_rerank"], "summary_answerable"),
            ],
        ),
        "",
        (
            "A correct refusal on an out-of-corpus question scores 1.0 on faithfulness (it "
            "invents nothing) and 0.0 on answer relevance (it does not answer), which is "
            "why the last two rows exclude those three cases. Both views are shown; neither "
            "is the headline on its own."
        ),
        "",
        "## By failure category (reranking on)",
        "",
        report.table(
            ("category", "n", "faithfulness", "answer relevance", "context precision", "context recall"),
            [
                [category, str(data["n"]), *(report.cell(data[m]) for m in METRICS)]
                for category, data in results["runs"]["rerank"]["by_category"].items()
            ],
        ),
        "",
    ]
    return "\n".join(out)


if __name__ == "__main__":
    asyncio.run(main())
