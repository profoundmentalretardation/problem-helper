"""The thing being judged: a RAG answer built from the retrieval layer.

This is deliberately *not* the hint pipeline. Agent-level evaluation is a separate exercise;
what Part 3 measures is the generation step on top of retrieval — retrieve k chunks, answer
from them, judge the answer against the context and the reference. Keeping it here rather
than in `problem_helper` is the point: the harness may not reach into the graph, and the
graph may not know it is being measured.

The answering prompt is held to the same rule the hint agent works under — never go beyond
the retrieved passages, and say so when they do not cover the question. That is what makes
the out-of-corpus cases meaningful: a system that quietly improvises scores well on
relevance and badly on faithfulness, which is exactly the trade the metrics should surface.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from problem_helper.llm import LLMProtocol
from problem_helper.retrieval import RetrievalService

from .judge import ResponseCache

ANSWER_SYSTEM = """\
You answer a programming student's question using only the passages you are given.

Rules:
- Use only what the passages say. Do not add facts from your own knowledge, however sure you
  are of them.
- When the passages do not answer the question, say plainly that the study library does not
  cover it. Do not improvise from loosely related passages.
- Answer in three to six sentences, concrete and specific. No filler and no preamble.\
"""


class Answer(BaseModel):
    answer: str = Field(description="The answer to the student, or a statement that the "
                                    "passages do not cover the question")
    used_passages: list[int] = Field(description="1-based indices of the passages actually used")


def format_contexts(contexts: list[str]) -> str:
    return "\n\n".join(f"[{i}] {c}" for i, c in enumerate(contexts, start=1))


async def answer_question(
    llm: LLMProtocol,
    *,
    model: str,
    question: str,
    contexts: list[str],
    cache: ResponseCache | None = None,
) -> Answer:
    """The answer under test, cached like the judgements so a re-run is reproducible."""
    user = f"# Question\n{question}\n\n# Passages\n{format_contexts(contexts)}"
    key = ResponseCache.key(model, "answer", ANSWER_SYSTEM + user) if cache else ""
    if cache is not None:
        hit = cache.get(key)
        if hit is not None:
            return Answer.model_validate(hit)
    answer = await llm.structured(
        model=model, system=ANSWER_SYSTEM, user=user, schema=Answer
    )
    if cache is not None:
        cache.put(key, answer.model_dump())
    return answer


def retrieve_contexts(
    service: RetrievalService, query: str, *, k: int, rerank: bool
) -> tuple[list[str], list[str]]:
    """The passages for one query, in retriever order, plus their chunk ids."""
    hits = service.search(query, k=k, rerank=rerank)
    texts = [f"{hit.chunk.title} — {hit.chunk.heading}\n{hit.chunk.text}" for hit in hits]
    return texts, [hit.chunk.id for hit in hits]
