"""Part 2: the rank-aware retrieval metrics over the hybrid retriever.

Free to run — no LLM call anywhere in this file — so it is the first thing to run and the
thing to re-run after any change to chunking, fusion or the reranker.

What one invocation produces:

* the five metrics at several `k`, for four pipeline stages (dense only, BM25 only, fused,
  fused + reranked), so "what did each stage buy" is a number rather than an impression;
* the same table per failure category, because an average over a mixed set hides which
  category the retriever is actually bad at;
* the LIM-packing trap, measured: the identical result set scored in retriever order and in
  packed order.

    uv run python -m evals.run_retrieval
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from problem_helper.retrieval import RetrievalParams, RetrievalService, pack_for_lim

from . import report
from .dataset import EvalCase, load, resolve_all
from .retrieval_metrics import aggregate, score_case

RESULTS_DIR = Path(__file__).parent / "results"
K_VALUES = (1, 3, 5, 10)
PRIMARY_K = 5
STAGES = ("dense", "bm25", "fused", "rerank")

logger = logging.getLogger(__name__)


def rankings_for(service: RetrievalService, cases: list[EvalCase], depth: int) -> dict[str, dict[str, list[str]]]:
    """Every stage's ranking for every case, retrieved once at the deepest k we score.

    Retrieving at `max(K_VALUES)` and truncating per k keeps the runs comparable: the same
    ranking is scored at every k instead of one search per k, which would let the reranker
    see a different candidate set each time.
    """
    params = RetrievalParams(
        top_k=depth,
        candidates=max(service.params.candidates, depth),
        rerank_depth=max(service.params.rerank_depth, depth),
        rrf_k=service.params.rrf_k,
        rerank=True,
    )
    rankings: dict[str, dict[str, list[str]]] = {}
    for case in cases:
        stages = service.explain(case.query, params=params)
        rankings[case.id] = {
            stage: [hit.chunk.id for hit in stages[stage]] for stage in STAGES
        }
    return rankings


def score_stage(
    cases: list[EvalCase],
    golden: dict[str, set[str]],
    rankings: dict[str, dict[str, list[str]]],
    stage: str,
    k: int,
) -> dict:
    per_case = {
        case.id: score_case(rankings[case.id][stage], golden[case.id], k) for case in cases
    }
    return {"summary": aggregate(per_case.values()), "per_case": per_case}


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    evalset = load()
    service = RetrievalService(cache_dir=Path(".rag_cache"))
    golden = resolve_all(evalset.cases, service.chunks)

    scored = [c for c in evalset.cases if not c.unanswerable]
    logger.info(
        "%s cases (%s scorable, %s out-of-corpus) over %s chunks",
        len(evalset.cases),
        len(scored),
        len(evalset.cases) - len(scored),
        len(service.chunks),
    )

    rankings = rankings_for(service, evalset.cases, depth=max(K_VALUES))

    results: dict = {
        "params": service.params.as_dict(),
        "chunks": len(service.chunks),
        "cases": len(evalset.cases),
        "unanswerable": len(evalset.cases) - len(scored),
        "by_k": {},
        "by_category": {},
        "packing": {},
        "rankings": rankings,
    }

    for k in K_VALUES:
        results["by_k"][k] = {
            stage: score_stage(evalset.cases, golden, rankings, stage, k)["summary"]
            for stage in STAGES
        }

    for category, cases in evalset.by_category().items():
        results["by_category"][category] = {
            "n": len(cases),
            "unanswerable": sum(c.unanswerable for c in cases),
            "fused": score_stage(cases, golden, rankings, "fused", PRIMARY_K)["summary"],
            "rerank": score_stage(cases, golden, rankings, "rerank", PRIMARY_K)["summary"],
        }

    # The trap: identical chunks, presentation order instead of retriever order.
    for k in (PRIMARY_K, max(K_VALUES)):
        packed = {
            case.id: score_case(
                pack_for_lim(rankings[case.id]["rerank"][:k]), golden[case.id], k
            )
            for case in evalset.cases
        }
        results["packing"][k] = {
            "retriever_order": results["by_k"][k]["rerank"],
            "packed_order": aggregate(packed.values()),
        }

    results["per_case"] = {
        case.id: {
            "category": case.category,
            "query": case.query,
            "golden": sorted(golden[case.id]),
            "first_relevant_rank": {
                stage: _first_rank(rankings[case.id][stage], golden[case.id])
                for stage in STAGES
            },
        }
        for case in evalset.cases
    }

    RESULTS_DIR.mkdir(exist_ok=True)
    (RESULTS_DIR / "retrieval.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    (RESULTS_DIR / "retrieval.md").write_text(render(evalset, results), encoding="utf-8")
    logger.info("wrote %s", RESULTS_DIR / "retrieval.md")
    print(render(evalset, results))


def _first_rank(ranking: list[str], relevant: set[str]) -> int | None:
    for rank, chunk_id in enumerate(ranking, start=1):
        if chunk_id in relevant:
            return rank
    return None


def render(evalset, results: dict) -> str:
    params = results["params"]
    out = [
        "# Retrieval metrics",
        "",
        (
            f"{results['cases']} cases ({results['unanswerable']} out-of-corpus, excluded "
            f"from every average below), {results['chunks']} chunks."
        ),
        (
            f"Fusion `k={params['rrf_k']}`, {params['candidates']} candidates per "
            f"retriever, reranking the top {params['rerank_depth']}."
        ),
        "",
        "## By stage and k",
        "",
    ]
    for k in K_VALUES:
        rows = [report.metric_row(stage, results["by_k"][k][stage]) for stage in STAGES]
        out += [f"**k = {k}**", "", report.table(report.METRIC_HEADERS, rows), ""]

    out += ["## Reranking on vs. off, per failure category (k = 5)", ""]
    rows = []
    for category, data in sorted(results["by_category"].items()):
        for label, key in (("fused", "fused"), ("+rerank", "rerank")):
            summary = data[key]
            rows.append(
                [
                    f"{category} ({data['n']})" if key == "fused" else "",
                    label,
                    report.cell(summary["hit_rate"]),
                    report.cell(summary["recall"]),
                    report.cell(summary["mrr"]),
                    report.cell(summary["ndcg"]),
                ]
            )
    out += [
        report.table(("category (n)", "run", "hit@5", "recall@5", "MRR", "nDCG@5"), rows),
        "",
        "## The packing trap",
        "",
        (
            "The identical reranked chunks, scored in retriever order and after "
            "`pack_for_lim`. Hit rate, precision and recall cannot move — they do not read "
            "position — so a harness that scores the packed order looks fine in three "
            "metrics out of five."
        ),
        "",
    ]
    for k, data in results["packing"].items():
        out += [
            f"**k = {k}**",
            "",
            report.table(
                report.METRIC_HEADERS,
                [
                    report.metric_row("retriever order", data["retriever_order"]),
                    report.metric_row("LIM-packed order", data["packed_order"]),
                ],
            ),
            "",
        ]
    out += [
        "## First relevant chunk per case, by stage",
        "",
        report.table(
            ("case", "category", "dense", "bm25", "fused", "+rerank"),
            [
                [
                    case_id,
                    data["category"],
                    *(
                        str(data["first_relevant_rank"][stage] or "—")
                        for stage in STAGES
                    ),
                ]
                for case_id, data in results["per_case"].items()
            ],
        ),
        "",
        (
            "`—` means no golden chunk in the top 10; out-of-corpus cases have no golden "
            "chunk at all and show `—` everywhere by construction."
        ),
        "",
    ]
    return "\n".join(out)


if __name__ == "__main__":
    main()
