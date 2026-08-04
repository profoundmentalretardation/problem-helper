"""LangChain tools the hint agent may call, plus helpers to read back what it used.

The tools are registered with the framework (`@tool` → `BaseTool`) and bound to the model
in `graph.py`; nothing in the pipeline calls them directly. Whether they run at all is the
model's decision — for a plain off-by-one it usually answers straight away, for a mistake
that maps to a technique it pulls the matching material first.

Every tool returns a JSON string: models handle it more reliably than prose, and the graph
parses the same payloads back to build the reading list attached to the hint.
"""

from __future__ import annotations

import json
import logging

from langchain_core.messages import AnyMessage, ToolMessage
from langchain_core.tools import BaseTool, tool

from . import materials
from .schemas import MaterialRef

logger = logging.getLogger(__name__)


def _dump(payload: object) -> str:
    return json.dumps(payload, ensure_ascii=False)


@tool(parse_docstring=True)
def search_learning_materials(query: str, limit: int = 3) -> str:
    """Search the study library for materials about an algorithmic technique or mistake.

    Use it when the student's mistake maps to a standard technique (two pointers, binary
    search, prefix sums, parity, loop bounds, complexity) and you want the wording of the
    hint to line up with what the student can go and read afterwards.

    Args:
        query: English keywords describing the technique or the mistake, e.g.
            "binary search off-by-one" or "even numbers sum". Translate the query into
            English even when the problem statement is in another language.
        limit: How many materials to return, 1 to 5.

    Returns:
        A JSON list of materials with id, title, topic, level, tags and a one-line summary.
        Call get_learning_material with an id to read the full note.
    """
    found = materials.search(query, limit=min(max(limit, 1), 5))
    logger.info("tool search_learning_materials(%r) → %s hit(s)", query, len(found))
    return _dump(
        [
            {
                "id": m.id,
                "title": m.title,
                "topic": m.topic,
                "level": m.level,
                "tags": m.tags,
                "summary": m.summary,
            }
            for m in found
        ]
    )


@tool(parse_docstring=True)
def get_learning_material(material_id: str) -> str:
    """Read one study material in full, including the list of typical pitfalls.

    Args:
        material_id: The id returned by search_learning_materials, e.g. "algo-two-pointers".

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

    Use it to see what the library holds before searching, or when a keyword search comes
    back empty.

    Returns:
        A JSON object mapping every topic to the ids of its materials.
    """
    grouped: dict[str, list[str]] = {}
    for material in materials.CATALOG:
        grouped.setdefault(material.topic, []).append(material.id)
    return _dump(grouped)


TOOLS: list[BaseTool] = [
    search_learning_materials,
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
    reading list attached to a hint cannot contain a made-up id.
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
    return [item["id"] for item in items if isinstance(item, dict) and "id" in item]


def _known_ids() -> list[str]:
    return [m.id for m in materials.CATALOG]
