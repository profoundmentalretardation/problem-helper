---
id: algo-sorting-key
title: Sorting with a key function
topic: sorting
level: intermediate
tags: sort, key, stable, tuple, reverse, cmp_to_key
summary: `key=` sorts by a derived value; tuples give tie-breakers for free.
---

# Sorting with a key function

Python's `sorted` and `list.sort` take a `key` function that maps each element to the value
it should be ordered by. The key is computed once per element (not once per comparison), so
even an expensive key is cheap: `n` calls, not `n log n`.

## Multi-level ordering with tuples

Tuples compare lexicographically, which gives tie-breaking for free:

```python
students.sort(key=lambda s: (s.score, s.name))          # score, then name
```

For "highest score first, alphabetical on ties" the two levels want opposite directions.
`reverse=True` flips the *whole* comparison, including the name. Negate the numeric
component instead:

```python
students.sort(key=lambda s: (-s.score, s.name))         # correct
students.sort(key=lambda s: (s.score, s.name), reverse=True)   # also reverses names
```

Negation only works for numbers. For a descending string component, sort twice and lean on
stability instead.

## Stability

Python's sort is **stable**: elements that compare equal keep their relative order. Two
consequences worth knowing:

1. Sorting by the secondary key first and the primary key second produces the same result
   as one tuple key. That is the trick when one level cannot be negated.
2. The original input order is a usable final tie-break. If it matters, sort a list of
   `(index, item)` pairs or rely on stability rather than hoping.

```python
records.sort(key=lambda r: r.name)          # secondary
records.sort(key=lambda r: -r.score)        # primary; equal scores keep name order
```

## `sorted` versus `.sort()`

`sorted(x)` returns a new list and accepts any iterable — including a `dict`, where it
returns the sorted keys. `x.sort()` sorts in place and returns `None`. Writing
`a = a.sort()` sets `a` to `None`, which then fails somewhere else entirely.

Sort in place when the original order is not needed; use `sorted` when it is, or when the
input is a tuple, a set or a generator.

## `operator` instead of `lambda`

```python
from operator import itemgetter, attrgetter

pairs.sort(key=itemgetter(1))                  # by the second element
people.sort(key=attrgetter("last", "first"))   # two attributes, same as a tuple key
```

These are implemented in C and measurably faster than the equivalent lambda in hot loops.

## Comparison functions

When the ordering rule cannot be expressed as a key — the classic case is "arrange numbers
to form the largest concatenation" — wrap a comparator:

```python
from functools import cmp_to_key

def compare(x: str, y: str) -> int:
    if x + y > y + x:
        return -1          # x first
    if x + y < y + x:
        return 1
    return 0

parts.sort(key=cmp_to_key(compare))
```

Python 3 removed the `cmp=` parameter, so `cmp_to_key` is the only supported route. It is
slower than a key function because it calls back into Python for every comparison; prefer a
key whenever one exists.

## Cost and alternatives

`sort` is O(n log n) (Timsort — near-linear on partially ordered input). If you only need
the `k` largest, `heapq.nlargest(k, a)` is O(n log k), and for a single extreme value
`max`/`min` with the same `key=` argument is O(n) and clearer.

Sorting also destroys information: after sorting, positions no longer mean what they meant.
When the answer needs original indices, sort `enumerate(a)` or an index list
(`sorted(range(len(a)), key=a.__getitem__)`).

## Pitfalls

- **`a = a.sort()`** — in-place sort returns `None`.
- **`reverse=True` with a tuple key** when only one component should be descending.
- **Sorting when order is part of the problem.** Subarray and subsequence problems usually
  break the moment you sort.
- **Mixed types.** `sorted([1, "2"])` raises `TypeError`; parse the input to one type first.
- **A key with side effects.** It is called once per element in an unspecified order.
- **Sorting inside a loop.** Sort once before the loop; an O(n log n) call in an O(n) loop
  is O(n^2 log n).
- **Assuming `sorted(dict)` gives values** — it gives the keys.
