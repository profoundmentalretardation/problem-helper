"""The study library: markdown notes on algorithms, loaded from `corpus/`.

One file is one material. The frontmatter carries the metadata the tools show
(`id`, `title`, `topic`, `level`, `tags`, `summary`) and the body is the note itself,
written with `##` headings — those headings are the semantic boundary the retrieval layer
chunks on, so they are part of the data format, not decoration.

The catalog is read once at import and cached. `get`/`all`/`topics`/`sections` are the only
entry points, so replacing the directory with a content service later touches neither the
tools nor the graph. Search deliberately does **not** live here: it belongs to
`retrieval`, which indexes the sections this module hands out.
"""

from __future__ import annotations

import logging
from functools import lru_cache
from pathlib import Path

from pydantic import BaseModel, Field

logger = logging.getLogger(__name__)

CORPUS_DIR = Path(__file__).parent / "corpus"

_REQUIRED = ("id", "title", "topic", "level", "tags", "summary")


class Section:
    """One `##` section of a note — the unit the retrieval layer chunks.

    A plain class rather than a pydantic model: it is built thousands of times per index
    build and never crosses the API boundary.
    """

    __slots__ = ("heading", "index", "material_id", "text")

    def __init__(self, material_id: str, index: int, heading: str, text: str) -> None:
        self.material_id = material_id
        self.index = index
        self.heading = heading
        self.text = text

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"Section({self.material_id}#{self.index} {self.heading!r})"


class Material(BaseModel):
    """One study note: a short explanation of a technique with the pitfalls listed."""

    id: str
    title: str
    topic: str
    level: str = Field(description="beginner | intermediate")
    tags: list[str]
    summary: str
    body: str

    def sections(self) -> list[Section]:
        """The body split on `##` headings, in document order.

        Text before the first `##` (the intro under the `#` title) becomes section 0 with
        the material's own title as its heading, so nothing is dropped.
        """
        sections: list[Section] = []
        heading = self.title
        buffer: list[str] = []

        def flush() -> None:
            text = "\n".join(buffer).strip()
            if text:
                sections.append(Section(self.id, len(sections), heading, text))

        for line in self.body.splitlines():
            if line.startswith("## "):
                flush()
                heading = line[3:].strip()
                buffer = []
                continue
            if line.startswith("# "):  # the document title, already in `title`
                continue
            buffer.append(line)
        flush()
        return sections


def _parse(text: str, source: Path) -> Material:
    """Split `---` frontmatter from the body.

    The frontmatter is a flat `key: value` block — no nesting, no anchors — so it is parsed
    here instead of pulling in a YAML dependency for six keys.
    """
    if not text.startswith("---"):
        raise ValueError(f"{source.name}: missing frontmatter")
    _, front, body = text.split("---", 2)

    fields: dict[str, object] = {}
    for line in front.strip().splitlines():
        key, _, value = line.partition(":")
        key, value = key.strip(), value.strip()
        if not key:
            continue
        fields[key] = [t.strip() for t in value.split(",") if t.strip()] if key == "tags" else value

    missing = [key for key in _REQUIRED if key not in fields]
    if missing:
        raise ValueError(f"{source.name}: frontmatter is missing {', '.join(missing)}")
    return Material(**fields, body=body.strip())  # type: ignore[arg-type]


@lru_cache(maxsize=1)
def _catalog() -> tuple[Material, ...]:
    materials = [
        _parse(path.read_text(encoding="utf-8"), path)
        for path in sorted(CORPUS_DIR.glob("*.md"))
    ]
    ids = [m.id for m in materials]
    duplicates = {i for i in ids if ids.count(i) > 1}
    if duplicates:
        raise ValueError(f"duplicate material ids in the corpus: {sorted(duplicates)}")
    logger.info("loaded %s materials from %s", len(materials), CORPUS_DIR)
    return tuple(materials)


def all() -> list[Material]:
    return list(_catalog())


def get(material_id: str) -> Material | None:
    return next((m for m in _catalog() if m.id == material_id), None)


def topics() -> list[str]:
    return sorted({m.topic for m in _catalog()})


def sections() -> list[Section]:
    """Every section of every material, in catalog order — the retrieval layer's input."""
    return [section for material in _catalog() for section in material.sections()]
