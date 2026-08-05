"""Part 3: LLM-as-judge scorers for the four generation metrics.

**Why hand-rolled rather than DeepEval or Ragas.** The service already owns a hardened
provider client — `LLMClient.structured` does strict `json_schema` with a schema-in-prompt
fallback, a retry on malformed JSON and a `LLMProtocol` that the test suite replaces with a
fake. A framework would bring a second provider stack, a second set of retry semantics and
its own embedding requirement (Ragas computes answer relevance through embeddings), for
metric definitions that are twenty lines each. Hand-rolling also makes the two things this
homework actually grades — caching and testability — trivial: every judgement is keyed and
stored on disk, and every scorer is a pure function of a pydantic answer, so the unit tests
score fixtures through `FakeLLM` with no network.

**The four metrics.** Each one asks the judge for a *decomposed* verdict and computes the
number in Python, rather than asking for a score out of ten:

| metric | asks the judge | computed here |
|---|---|---|
| faithfulness | split the answer into claims, mark each supported by the context | supported / total |
| answer relevance | does the answer address the question, verdict per requirement | satisfied / total |
| context precision | is each retrieved chunk useful for answering | rank-weighted average precision |
| context recall | is each sentence of the golden answer attributable to the context | attributable / total |

**Bias controls**, and what each one is worth here:

- *Position bias* — no pairwise comparisons anywhere; contexts are judged one at a time and
  the rank weighting is arithmetic done in Python, so the order the judge sees cannot
  influence the ordering it is scoring.
- *Verbosity bias* — the ratios above are per-claim, not holistic, so padding an answer adds
  claims that must each survive the context check. A long, waffly answer is scored *down* by
  faithfulness, which is the opposite of the usual failure.
- *Self-preference* — the judge model is deliberately from a different family than the
  answering model (see `run_generation.py`); a model grading its own output is the one
  configuration this harness refuses to run.
- *Judge-model mismatch* — the remaining exposure. The judge is a small, cheap model, and on
  the borderline cases its claim decomposition is coarser than a human's. It is documented
  rather than solved: with 27 cases the metric is a comparison between two runs of the same
  pipeline under the same judge, not an absolute quality score.
"""

from __future__ import annotations

import hashlib
import json
import logging
from pathlib import Path

from pydantic import BaseModel, Field

from problem_helper.llm import LLMProtocol

logger = logging.getLogger(__name__)

CACHE_PATH = Path(__file__).parent / "results" / "response_cache.json"


# --------------------------------------------------------------------------- #
# Judge answer schemas — strict mode: no optional fields, no defaults
# --------------------------------------------------------------------------- #


class ClaimVerdict(BaseModel):
    claim: str = Field(description="One atomic factual claim taken from the answer")
    supported: bool = Field(description="True only if the context states or entails it")
    reason: str = Field(description="Short justification, quoting the context when supported")


class FaithfulnessAnswer(BaseModel):
    claims: list[ClaimVerdict]


class RequirementVerdict(BaseModel):
    requirement: str = Field(description="One thing the question asks for")
    satisfied: bool = Field(description="True if the answer addresses it")
    reason: str


class RelevanceAnswer(BaseModel):
    requirements: list[RequirementVerdict]
    evasive: bool = Field(description="True if the answer dodges the question or rambles")


class ContextVerdict(BaseModel):
    useful: bool = Field(description="True if this passage helps answer the question")
    reason: str


class SentenceVerdict(BaseModel):
    sentence: str = Field(description="One sentence of the reference answer")
    attributable: bool = Field(description="True if the context supports it")
    reason: str


class ContextRecallAnswer(BaseModel):
    sentences: list[SentenceVerdict]


# --------------------------------------------------------------------------- #
# Cache
# --------------------------------------------------------------------------- #


class ResponseCache:
    """A JSON file keyed by (model, kind, prompt digest).

    It holds every model response the harness makes — the judgements *and* the answers under
    test. Caching only the judgements is not enough to make a run reproducible: the answering
    model is sampled, so a re-run produces a different answer, which is a different judge
    prompt, which is a cache miss on all four metrics. With both cached, a second run is free
    and byte-identical, and the numbers in the README can be regenerated without paying for
    them twice.
    """

    def __init__(self, path: Path = CACHE_PATH) -> None:
        self.path = path
        self.entries: dict[str, dict] = {}
        if path.exists():
            self.entries = json.loads(path.read_text(encoding="utf-8"))
        self.hits = 0
        self.misses = 0

    @staticmethod
    def key(model: str, metric: str, payload: str) -> str:
        digest = hashlib.sha256(payload.encode()).hexdigest()[:20]
        return f"{model}|{metric}|{digest}"

    def get(self, key: str) -> dict | None:
        entry = self.entries.get(key)
        self.hits += entry is not None
        self.misses += entry is None
        return entry

    def put(self, key: str, value: dict) -> None:
        self.entries[key] = value

    def save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.write_text(json.dumps(self.entries, indent=2, ensure_ascii=False), encoding="utf-8")


# --------------------------------------------------------------------------- #
# Prompts
# --------------------------------------------------------------------------- #

FAITHFULNESS_SYSTEM = """\
You check whether an answer is grounded in the context it was given.

Break the answer into atomic factual claims — one verifiable statement each, no compound
sentences. For every claim decide whether the CONTEXT states it or directly entails it.

Rules:
- Judge grounding only. A claim can be true in the world and still unsupported here.
- Ignore length, tone and style entirely.
- An answer that says the context does not cover the question makes no factual claim about
  the subject: return that single claim and mark it supported if the context indeed lacks
  the answer.\
"""

RELEVANCE_SYSTEM = """\
You check whether an answer addresses the question that was asked.

List what the question actually asks for — usually one or two things, at most four — and for
each decide whether the answer addresses it.

Rules:
- Judge relevance only, never correctness or grounding.
- Ignore length and style. A short direct answer is not worse than a long one.
- Mark `evasive` when the answer talks around the question, answers a different one, or
  pads with unrelated material. Correctly refusing a question the context cannot answer is
  NOT evasive.\
"""

CONTEXT_PRECISION_SYSTEM = """\
You judge whether one retrieved passage is useful for answering a question.

Useful means a person writing the answer would draw on this passage. A passage on a related
topic that contributes nothing to this particular question is not useful.

Judge this passage alone. Do not speculate about the other passages.\
"""

CONTEXT_RECALL_SYSTEM = """\
You check whether a reference answer could have been written from the given context.

Split the reference answer into sentences and for each decide whether the context supports
it — states it, or entails it closely enough to write that sentence.

Judge attribution only, never style or completeness of the context as a whole.\
"""


# --------------------------------------------------------------------------- #
# Scorers
# --------------------------------------------------------------------------- #


class Judge:
    """The four scorers over one `LLMProtocol`. Every call goes through the cache."""

    def __init__(self, llm: LLMProtocol, *, model: str, cache: ResponseCache | None = None) -> None:
        self._llm = llm
        self._model = model
        self.cache = cache if cache is not None else ResponseCache()

    async def _ask(self, metric: str, system: str, user: str, schema: type[BaseModel]) -> BaseModel:
        key = ResponseCache.key(self._model, metric, system + user)
        cached = self.cache.get(key)
        if cached is not None:
            return schema.model_validate(cached)
        answer = await self._llm.structured(
            model=self._model, system=system, user=user, schema=schema
        )
        self.cache.put(key, answer.model_dump())
        return answer

    # -- metrics ------------------------------------------------------- #

    async def faithfulness(self, *, question: str, answer: str, contexts: list[str]) -> dict:
        """Supported claims / total claims. 1.0 for an answer that claims nothing."""
        verdict: FaithfulnessAnswer = await self._ask(
            "faithfulness",
            FAITHFULNESS_SYSTEM,
            _block(question=question, context=_join(contexts), answer=answer),
            FaithfulnessAnswer,
        )
        claims = verdict.claims
        score = (sum(c.supported for c in claims) / len(claims)) if claims else 1.0
        return {
            "score": score,
            "claims": len(claims),
            "unsupported": [c.claim for c in claims if not c.supported],
        }

    async def answer_relevance(self, *, question: str, answer: str) -> dict:
        """Satisfied requirements / total, halved when the judge calls the answer evasive.

        The context is deliberately not shown: relevance is about the question, and a judge
        that sees the passages starts grading grounding instead.
        """
        verdict: RelevanceAnswer = await self._ask(
            "answer_relevance",
            RELEVANCE_SYSTEM,
            _block(question=question, answer=answer),
            RelevanceAnswer,
        )
        parts = verdict.requirements
        base = (sum(r.satisfied for r in parts) / len(parts)) if parts else 0.0
        return {
            "score": base * (0.5 if verdict.evasive else 1.0),
            "requirements": len(parts),
            "evasive": verdict.evasive,
            "missed": [r.requirement for r in parts if not r.satisfied],
        }

    async def context_precision(self, *, question: str, contexts: list[str]) -> dict:
        """Rank-weighted average precision over the retrieved passages.

        Each passage is judged on its own, then weighted by its position, so a useful chunk
        at rank 1 counts for more than the same chunk at rank 5 — the ranking is scored in
        Python, never by asking the judge to compare passages.
        """
        useful: list[bool] = []
        reasons: list[str] = []
        for position, context in enumerate(contexts, start=1):
            verdict: ContextVerdict = await self._ask(
                "context_precision",
                CONTEXT_PRECISION_SYSTEM,
                _block(question=question, passage=context),
                ContextVerdict,
            )
            useful.append(verdict.useful)
            if not verdict.useful:
                reasons.append(f"#{position}: {verdict.reason}")
        if not useful or not any(useful):
            return {"score": 0.0, "useful": 0, "of": len(useful), "rejected": reasons}
        precisions = [
            sum(useful[: i + 1]) / (i + 1) for i, is_useful in enumerate(useful) if is_useful
        ]
        return {
            "score": sum(precisions) / len(precisions),
            "useful": sum(useful),
            "of": len(useful),
            "rejected": reasons,
        }

    async def context_recall(self, *, golden_answer: str, contexts: list[str]) -> dict:
        """Attributable sentences of the reference answer / total."""
        verdict: ContextRecallAnswer = await self._ask(
            "context_recall",
            CONTEXT_RECALL_SYSTEM,
            _block(reference_answer=golden_answer, context=_join(contexts)),
            ContextRecallAnswer,
        )
        sentences = verdict.sentences
        score = (sum(s.attributable for s in sentences) / len(sentences)) if sentences else 0.0
        return {
            "score": score,
            "sentences": len(sentences),
            "unsupported": [s.sentence for s in sentences if not s.attributable],
        }


def _join(contexts: list[str]) -> str:
    return "\n\n".join(f"[{i}] {c}" for i, c in enumerate(contexts, start=1)) or "<empty>"


def _block(**parts: str) -> str:
    return "\n\n".join(f"# {name.replace('_', ' ').title()}\n{value}" for name, value in parts.items())
