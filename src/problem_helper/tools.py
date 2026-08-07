"""LangChain tools the hint agent may call, plus helpers to read back what it used.

The tools are registered with the framework (`@tool` → `BaseTool`) and bound to the model
in `graph.py`; nothing in the pipeline calls them directly. Whether they run at all is the
model's decision — for a plain off-by-one it usually answers straight away, for a mistake
that maps to a technique it searches first, and it is free to search again with a different
query when the first result set misses.

`search_corpus` is the retrieval layer's only entry point into the agent. It is a tool
rather than a step in front of the graph on purpose: a hint for a typo needs no corpus at
all, and paying for retrieval on every session would be both slower and worse.

Every tool returns a JSON string: models handle it more reliably than prose, and the graph
parses the same payloads back to build the reading list attached to the hint.
"""

from __future__ import annotations

import json
import logging

from langchain_core.messages import AnyMessage, ToolMessage
from langchain_core.tools import BaseTool, tool

from . import materials
from .retrieval import get_retriever, pack_for_lim
from .schemas import MaterialRef

logger = logging.getLogger(__name__)

EXCERPT_CHARS = 700


def _dump(payload: object) -> str:
    return json.dumps(payload, ensure_ascii=False)


@tool(parse_docstring=True)
def search_corpus(query: str, k: int = 5) -> str:
    """Search the study library for passages about a technique, a mistake or an API.

    The search is hybrid: a vector index finds passages that mean the same thing as the
    query, a lexical index finds the ones that use its exact words (identifiers like
    `bisect_left`, acronyms like BFS or DP), the two rankings are fused, and a reranker
    orders what comes out. Ask in natural language, not in keywords.

    Search again with a different wording when the results miss the point — a second query
    is cheap and is often the difference between a generic hint and a precise one.

    Args:
        query: What you want to know, in English, e.g. "why does my BFS visit a node twice"
            or "difference between bisect_left and bisect_right". Translate the query into
            English even when the problem statement is in another language.
        k: How many passages to return, 1 to 10.

    Returns:
        A JSON list of passages, each with the id of the material it belongs to, its
        heading, its rank and an excerpt. Call get_learning_material with a material_id to
        read that note in full.
    """
    hits = get_retriever().search(query, k=min(max(k, 1), 10))
    logger.info("tool search_corpus(%r) → %s passage(s)", query, len(hits))
    payload = [
        {
            "rank": rank,
            "chunk_id": hit.chunk.id,
            "material_id": hit.chunk.material_id,
            "title": hit.chunk.title,
            "heading": hit.chunk.heading,
            "excerpt": hit.chunk.text[:EXCERPT_CHARS],
        }
        for rank, hit in enumerate(hits, start=1)
    ]
    # The rank field carries the ranking; the list order is packed strongest-at-the-edges,
    # because that is where a long context is actually read. See retrieval/packing.py.
    return _dump(pack_for_lim(payload))


@tool(parse_docstring=True)
def get_learning_material(material_id: str) -> str:
    """Read one study material in full, including the list of typical pitfalls.

    Args:
        material_id: The id returned by search_corpus, e.g. "algo-two-pointers".

    Returns:
        A JSON object with the full text of the material, or an error field when the id is
        unknown.
    """
    material = materials.get(material_id)
    logger.info("tool get_learning_material(%r) → %s", material_id, bool(material))
    if material is None:
        return _dump(
            {"error": f"unknown material_id {material_id!r}", "known_ids": _known_ids()}
        )
    return _dump(material.model_dump())


@tool(parse_docstring=True)
def list_material_topics() -> str:
    """List the topics covered by the study library, with the material ids under each.

    Use it to see what the library holds before searching, or when a search comes back with
    nothing that fits.

    Returns:
        A JSON object mapping every topic to the ids of its materials.
    """
    grouped: dict[str, list[str]] = {}
    for material in materials.all():
        grouped.setdefault(material.topic, []).append(material.id)
    return _dump(grouped)


TOOLS: list[BaseTool] = [
    search_corpus,
    get_learning_material,
    list_material_topics,
]


def specs() -> list[dict]:
    """What `GET /v1/tools` shows: the registered tools and their argument schemas."""
    return [
        {
            "name": t.name,
            "description": (t.description or "").strip(),
            "args": t.args,
        }
        for t in TOOLS
    ]


def read_materials(messages: list[AnyMessage]) -> list[MaterialRef]:
    """The materials the agent actually pulled, in the order it first saw them.

    Reconstructed from the tool results rather than from the model's own summary, so the
    reading list attached to a hint cannot contain a made-up id. Retrieval hits name a
    `material_id`; `get_learning_material` returns the material itself under `id`.
    """
    refs: dict[str, MaterialRef] = {}
    for message in messages:
        if not isinstance(message, ToolMessage):
            continue
        for material_id in _ids_in(message.content):
            material = materials.get(material_id)
            if material is not None and material.id not in refs:
                refs[material.id] = MaterialRef(
                    id=material.id,
                    title=material.title,
                    topic=material.topic,
                    summary=material.summary,
                )
    return list(refs.values())


def _ids_in(content: object) -> list[str]:
    if not isinstance(content, str):
        return []
    try:
        payload = json.loads(content)
    except json.JSONDecodeError:
        return []
    items = payload if isinstance(payload, list) else [payload]
    return [
        item["material_id"] if "material_id" in item else item["id"]
        for item in items
        if isinstance(item, dict) and ("material_id" in item or "id" in item)
    ]


def _known_ids() -> list[str]:
    return [m.id for m in materials.all()]
