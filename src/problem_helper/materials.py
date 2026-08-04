"""A tiny in-repo library of learning materials the hint agent may pull from.

The catalog is deliberately static: the MVP has no content service behind it, and a local
list keeps the tools deterministic in tests. `search`/`get` are the only entry points, so
swapping this module for a real HTTP client later does not touch the tools or the graph.
"""

from __future__ import annotations

import re

from pydantic import BaseModel, Field


class Material(BaseModel):
    """One study note: a short explanation of a technique with the pitfalls listed."""

    id: str
    title: str
    topic: str
    level: str = Field(description="beginner | intermediate")
    tags: list[str]
    summary: str
    body: str


CATALOG: list[Material] = [
    Material(
        id="algo-two-pointers",
        title="Two pointers on a sorted array",
        topic="two_pointers",
        level="beginner",
        tags=["array", "sorted", "pair sum", "linear scan"],
        summary="Walk two indices towards each other to find a pair in O(n) instead of O(n^2).",
        body=(
            "Keep `left` at the start and `right` at the end of a sorted array. Compare the "
            "current sum with the target: too small moves `left` forward, too large moves "
            "`right` back. Each step discards a whole row of the pair matrix, so the scan is "
            "linear.\n"
            "Pitfalls: the array must be sorted first; the loop condition is `left < right`, "
            "not `left <= right`, otherwise an element pairs with itself."
        ),
    ),
    Material(
        id="algo-sliding-window",
        title="Sliding window over a sequence",
        topic="sliding_window",
        level="beginner",
        tags=["subarray", "substring", "window", "prefix"],
        summary="Maintain a running aggregate over a moving range instead of recomputing it.",
        body=(
            "Extend the window on the right, and shrink it from the left while it violates the "
            "constraint. The aggregate (sum, count of distinct values) is updated when an "
            "element enters or leaves, never recomputed from scratch.\n"
            "Pitfalls: forgetting to subtract the element that leaves the window; shrinking "
            "with `if` where the invariant needs a `while`."
        ),
    ),
    Material(
        id="algo-binary-search",
        title="Binary search and its boundaries",
        topic="binary_search",
        level="beginner",
        tags=["search", "sorted", "off-by-one", "logarithmic"],
        summary="Halve the search range each step; most bugs live in the boundary update.",
        body=(
            "With `low, high = 0, n - 1` the loop runs while `low <= high` and the halves are "
            "`mid - 1` / `mid + 1`. With the half-open form `low, high = 0, n` the loop runs "
            "while `low < high` and the update is `high = mid`. Mixing the two forms is the "
            "classic source of infinite loops.\n"
            "Pitfalls: computing `mid` outside the loop; returning `mid` when the task asks "
            "for an insertion point."
        ),
    ),
    Material(
        id="algo-prefix-sums",
        title="Prefix sums for range queries",
        topic="prefix_sums",
        level="intermediate",
        tags=["sum", "range", "precomputation", "cumulative"],
        summary="Precompute cumulative sums so any range sum is one subtraction.",
        body=(
            "Build `pref[0] = 0` and `pref[i + 1] = pref[i] + a[i]`. The sum of `a[l..r]` "
            "(inclusive) is then `pref[r + 1] - pref[l]`. The extra leading zero is what makes "
            "the formula work for `l = 0`.\n"
            "Pitfalls: an array of length `n` instead of `n + 1`; mixing inclusive and "
            "exclusive bounds in the same expression."
        ),
    ),
    Material(
        id="algo-hash-counting",
        title="Counting with a dictionary",
        topic="hash_map",
        level="beginner",
        tags=["frequency", "dict", "counter", "duplicates"],
        summary="A dict from value to count turns repeated scans into a single pass.",
        body=(
            "Fill `counts[value] = counts.get(value, 0) + 1` (or `collections.Counter`) in one "
            "pass, then answer questions about duplicates, the most frequent element or "
            "complements in O(1) per lookup.\n"
            "Pitfalls: comparing counts of unhashable values; iterating over a dict while "
            "mutating it; assuming insertion order equals sorted order."
        ),
    ),
    Material(
        id="algo-parity-filters",
        title="Filtering by parity and other predicates",
        topic="basics",
        level="beginner",
        tags=["even", "odd", "modulo", "filter", "sum"],
        summary="`x % 2 == 0` selects even numbers; the sign of the remainder bites in other bases.",
        body=(
            "A parity filter is `x % 2 == 0` for even and `x % 2 != 0` for odd. In Python the "
            "result of `%` carries the sign of the divisor, so `-3 % 2` is `1` — negative "
            "numbers still classify correctly, unlike in C.\n"
            "Pitfalls: inverting the condition; summing the indices instead of the values; "
            "starting the accumulator at something other than 0."
        ),
    ),
    Material(
        id="algo-loop-bounds",
        title="Loop bounds and off-by-one errors",
        topic="basics",
        level="beginner",
        tags=["range", "off-by-one", "index", "loop"],
        summary="`range(n)` stops at n - 1; comparing neighbours needs one iteration less.",
        body=(
            "`range(a, b)` yields `a .. b - 1`. Comparing `a[i]` with `a[i + 1]` must therefore "
            "loop over `range(len(a) - 1)`, otherwise the last step reads past the end. When "
            "the task counts from 1, convert once at the boundary instead of sprinkling "
            "`- 1` through the body.\n"
            "Pitfalls: `range(1, n)` when the first element matters; an empty input making "
            "`len(a) - 1` negative."
        ),
    ),
    Material(
        id="algo-stdin-parsing",
        title="Reading stdin in competitive problems",
        topic="io",
        level="beginner",
        tags=["input", "stdin", "parsing", "split", "int"],
        summary="Read the numbers with split() and map(int, ...), and mind the declared count.",
        body=(
            "A typical layout is a line with `n` followed by a line with `n` numbers: "
            "`n = int(input())` then `values = list(map(int, input().split()))`. When the "
            "numbers may span several lines, read everything with `sys.stdin.read().split()`.\n"
            "Pitfalls: trusting `n` instead of `len(values)`; leaving a trailing newline in a "
            "string comparison; printing a list instead of the elements."
        ),
    ),
    Material(
        id="algo-sorting-key",
        title="Sorting with a key function",
        topic="sorting",
        level="intermediate",
        tags=["sort", "key", "stable", "tuple"],
        summary="`key=` sorts by a derived value; tuples give tie-breakers for free.",
        body=(
            "`sorted(items, key=lambda x: (x.score, x.name))` sorts by score and breaks ties by "
            "name. Python's sort is stable, so sorting twice by different keys keeps the order "
            "of the first sort inside equal groups. `reverse=True` flips the whole comparison, "
            "not a single component — negate a numeric key instead.\n"
            "Pitfalls: passing a comparison function instead of a key; sorting in place with "
            "`.sort()` when the original order is still needed."
        ),
    ),
    Material(
        id="algo-complexity",
        title="Estimating time complexity",
        topic="complexity",
        level="intermediate",
        tags=["big-o", "performance", "timeout", "nested loops"],
        summary="Count the nested loops before optimising: 1e8 simple steps is the practical ceiling.",
        body=(
            "Two nested loops over `n` elements are O(n^2): at n = 10^5 that is 10^10 steps and "
            "a guaranteed timeout. Typical rewrites are a hash map (O(n)), sorting plus two "
            "pointers (O(n log n)) or prefix sums (O(n) after precomputation).\n"
            "Pitfalls: hidden loops inside `in` on a list or `list.remove`; string concatenation "
            "in a loop, which copies the whole string every time."
        ),
    ),
]

_BY_ID: dict[str, Material] = {m.id: m for m in CATALOG}


def get(material_id: str) -> Material | None:
    return _BY_ID.get(material_id)


def topics() -> list[str]:
    return sorted({m.topic for m in CATALOG})


def search(query: str, limit: int = 3) -> list[Material]:
    """Keyword search over the catalog, best match first.

    Scoring is deliberately naive — a word hit in the tags or the title weighs more than one
    in the body — but it is stable and needs no index.
    """
    words = _words(query)
    if not words:
        return []

    scored: list[tuple[int, int, Material]] = []
    for position, material in enumerate(CATALOG):
        score = _score(material, words)
        if score:
            scored.append((-score, position, material))
    scored.sort()
    return [material for _, _, material in scored[: max(1, limit)]]


def _words(text: str) -> list[str]:
    return [w for w in re.split(r"[^\w]+", text.lower()) if len(w) > 2]


def _score(material: Material, words: list[str]) -> int:
    tags = " ".join(material.tags).lower()
    title = f"{material.title} {material.topic}".lower()
    body = f"{material.summary} {material.body}".lower()
    return sum(
        4 * (word in tags) + 3 * (word in title) + (word in body) for word in set(words)
    )
