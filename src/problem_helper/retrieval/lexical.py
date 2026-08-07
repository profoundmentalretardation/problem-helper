"""BM25 over the chunks — the lexical half of the hybrid retriever.

Implemented here rather than pulled from a package for two reasons: the ranking function is
forty lines, and the *tokenizer* is the part that decides whether `bisect_left`, `BM25` or
`popleft` survive as query terms. A library tokenizer tuned for prose drops exactly those.

Tokenization rules, and why each one exists:

- lowercase, split on anything that is not a letter/digit/underscore — `deque.popleft()`
  becomes `deque`, `popleft`;
- an identifier with underscores is emitted whole *and* in parts (`bisect_left` →
  `bisect_left`, `bisect`, `left`), so both "bisect_left" and "bisect left" hit it;
- a trailing plural `s` is stripped ("pointers" → "pointer"), except after `s`, `u` or `i`
  where the rule does more harm than good (`class`, `bonus`, `axis`);
- no stemming beyond that and no stopword list. The corpus is small and technical: the
  aggressive stemmers turn `sorting`/`sorted`/`sort` into one term, which is fine, but they
  also mangle short identifiers, and stopwords like "in" and "is" are real Python operators
  here.

Scoring is textbook Okapi BM25 with `k1=1.5`, `b=0.75` — the standard defaults, kept
because the chunks are near-uniform in length, which is the regime those values were tuned
for. `b` controls length normalisation and matters little when every document is ~600
characters.
"""

from __future__ import annotations

import math
import re
from collections import Counter

from .chunking import Chunk

K1 = 1.5
B = 0.75

_TOKEN = re.compile(r"[a-z0-9_]+")


def tokenize(text: str) -> list[str]:
    """Text → the terms the index stores. See the module docstring for the rules."""
    tokens: list[str] = []
    for raw in _TOKEN.findall(text.lower()):
        tokens.append(_singular(raw))
        if "_" in raw:
            tokens.extend(_singular(part) for part in raw.split("_") if part)
    return tokens


def _singular(token: str) -> str:
    if len(token) > 3 and token.endswith("s") and token[-2] not in "sui":
        return token[:-1]
    return token


class Bm25Index:
    """An in-memory BM25 index. Rebuilt from the chunks in well under a second."""

    def __init__(self, chunks: list[Chunk]) -> None:
        self._chunks = chunks
        self._tf: list[Counter[str]] = [Counter(tokenize(c.indexed_text)) for c in chunks]
        self._lengths = [sum(tf.values()) for tf in self._tf]
        self._avg_length = (sum(self._lengths) / len(self._lengths)) if self._lengths else 0.0

        document_frequency: Counter[str] = Counter()
        for tf in self._tf:
            document_frequency.update(tf.keys())
        total = len(chunks)
        # Robertson/Sparck-Jones idf with the +0.5 smoothing; the outer +1 keeps it
        # non-negative for terms that appear in almost every chunk.
        self._idf = {
            term: math.log(1 + (total - freq + 0.5) / (freq + 0.5))
            for term, freq in document_frequency.items()
        }

    def search(self, query: str, limit: int) -> list[tuple[Chunk, float]]:
        terms = tokenize(query)
        if not terms:
            return []
        scored: list[tuple[float, int]] = []
        for index, tf in enumerate(self._tf):
            score = 0.0
            for term in terms:
                frequency = tf.get(term)
                if not frequency:
                    continue
                norm = 1 - B + B * (self._lengths[index] / (self._avg_length or 1))
                score += self._idf[term] * frequency * (K1 + 1) / (frequency + K1 * norm)
            if score > 0:
                scored.append((score, index))
        scored.sort(key=lambda pair: (-pair[0], pair[1]))
        return [(self._chunks[index], score) for score, index in scored[:limit]]
