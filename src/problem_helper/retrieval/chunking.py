"""Turning the study notes into retrievable chunks.

Strategy — structural first, size-based only where the structure is not enough:

1. **Split on `##` headings.** The notes are written so that one section is one idea
   ("The two consistent forms", "Pitfalls"), which is also the granularity a student's
   question lands on. Splitting there means a chunk never straddles two techniques, and the
   heading is a real title rather than a guess.
2. **Split the long sections further** with `RecursiveCharacterTextSplitter`. The measured
   section length over the corpus is 152–1351 characters with a median of 554, so most
   sections are already the size we want and only ~20% need the second pass. The splitter
   is set to 700/120: 700 keeps a chunk inside the reranker's comfortable window while
   still holding a whole code block, and the 120-character overlap carries the sentence
   that introduces a snippet into the chunk that contains it.
3. **Prefix every chunk with `title — heading`.** A chunk taken from the middle of a
   "Pitfalls" list has no other clue what it is about; both the embedding and the lexical
   index see that prefix, which is what makes a bare query like "off-by-one" reach the
   right document.

Chunk ids are positional (`{material_id}#{section}.{part}`) and therefore *not* stable
across a re-chunk — which is exactly why the eval set anchors on `(material_id, heading)`
and resolves ids at load time.
"""

from __future__ import annotations

from dataclasses import dataclass

from langchain_text_splitters import RecursiveCharacterTextSplitter

from .. import materials

CHUNK_SIZE = 700
CHUNK_OVERLAP = 120
SPLIT_THRESHOLD = 800
"""Sections shorter than this stay whole — splitting them would only lose context."""


@dataclass(frozen=True, slots=True)
class Chunk:
    """One indexed unit: a piece of a section, with its provenance attached."""

    id: str
    material_id: str
    title: str
    topic: str
    heading: str
    text: str
    tags: tuple[str, ...]

    @property
    def indexed_text(self) -> str:
        """What the embedder and BM25 actually see."""
        return f"{self.title} — {self.heading}\n\n{self.text}"

    @property
    def anchor(self) -> tuple[str, str]:
        """The stable identity of this chunk's source, used by the eval set."""
        return self.material_id, self.heading


def build_chunks() -> list[Chunk]:
    """Chunk the whole study library, in catalog order."""
    splitter = RecursiveCharacterTextSplitter(
        chunk_size=CHUNK_SIZE,
        chunk_overlap=CHUNK_OVERLAP,
        separators=["\n\n", "\n", ". ", " ", ""],
    )
    by_id = {m.id: m for m in materials.all()}
    chunks: list[Chunk] = []
    for section in materials.sections():
        material = by_id[section.material_id]
        pieces = (
            splitter.split_text(section.text)
            if len(section.text) > SPLIT_THRESHOLD
            else [section.text]
        )
        for part, text in enumerate(pieces):
            chunks.append(
                Chunk(
                    id=f"{material.id}#{section.index}.{part}",
                    material_id=material.id,
                    title=material.title,
                    topic=material.topic,
                    heading=section.heading,
                    text=text,
                    tags=tuple(material.tags),
                )
            )
    return chunks
