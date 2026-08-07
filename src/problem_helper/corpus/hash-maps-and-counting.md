---
id: algo-hash-counting
title: Counting with a dictionary
topic: hash_map
level: beginner
tags: frequency, dict, Counter, duplicates, complement, defaultdict
summary: A dict from value to count turns repeated scans into a single pass.
---

# Counting with a dictionary

A dictionary answers "have I seen this before" and "how many times" in O(1) per lookup. It
is the standard way to collapse an O(n^2) double loop into a single O(n) pass, and it is
the first thing to reach for when a problem mentions duplicates, frequencies, anagrams or
complements.

## Frequency tables

```python
counts = {}
for value in a:
    counts[value] = counts.get(value, 0) + 1
```

`dict.get(key, 0)` avoids the `KeyError` on the first occurrence. Two shorter forms do the
same thing:

```python
from collections import Counter, defaultdict

counts = Counter(a)                    # value -> how many times it appears
groups = defaultdict(list)             # key -> list of items, no initialisation needed
for word in words:
    groups["".join(sorted(word))].append(word)     # anagram grouping
```

`Counter` adds the operations that make it worth importing: `most_common(k)` returns the
top `k` pairs already sorted, `+` and `-` merge tables, and `&` intersects them (useful for
"can I build this word from those letters"). Note that `Counter` subtraction drops
non-positive counts, while `Counter.subtract` keeps them — including negatives.

`defaultdict(int)` behaves like `Counter` for counting but silently inserts a key on *read*,
which matters when you later iterate over the dictionary and find entries you never
intentionally added.

## The complement trick

The pair-sum problem in one pass, no sorting required:

```python
def two_sum(a: list[int], target: int) -> tuple[int, int] | None:
    seen = {}                              # value -> index
    for i, value in enumerate(a):
        if target - value in seen:
            return seen[target - value], i
        seen[value] = i                    # insert AFTER the lookup
    return None
```

Inserting before the lookup lets an element pair with itself and reports `i, i`. This is
also why the two-pointer version needs sorted input and this one does not: the dictionary
replaces the ordering property with memory.

The same shape solves "subarray with a given sum" by storing prefix sums instead of values,
and "longest subarray with sum k" by storing the *first* index at which each prefix sum
appeared.

## Sets when the count does not matter

If you only need membership, use a `set`. It is the same hash table without the values, and
the intent is clearer.

```python
seen = set()
for value in a:
    if value in seen:
        return value            # the first duplicate
    seen.add(value)
```

`x in some_list` is O(n) and `x in some_set` is O(1). Swapping one for the other inside a
loop is the single most common way an otherwise correct solution times out.

## Keys must be hashable

Lists and dictionaries cannot be keys; tuples and frozensets can. Convert before storing:

```python
seen.add(tuple(row))                    # a list would raise TypeError
seen.add(frozenset(members))            # order-insensitive key
```

Sorting a string into a canonical form (`"".join(sorted(word))`) is the usual key for
anagram problems. For coordinates, a `(row, col)` tuple is both hashable and readable —
avoid encoding them as `row * width + col` unless memory really demands it.

## Ordering

Since Python 3.7 a `dict` preserves **insertion** order. That is not sorted order, and
relying on it as if it were is a silent wrong answer. When the output must be sorted, sort
explicitly:

```python
for key in sorted(counts):                      # by key
    ...
for key, n in sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])):   # by count desc, then key
    ...
```

The `(-count, key)` tuple is the standard tie-break for "most frequent, alphabetical on
ties".

## Pitfalls

- **Mutating a dictionary while iterating it.** `RuntimeError: dictionary changed size
  during iteration`. Iterate over `list(counts.items())` or build a new dict.
- **Using `in` on a list inside a loop.** O(n) per check; convert to a set once, outside.
- **Assuming insertion order equals sorted order.**
- **`defaultdict` inserting on read.** `if d[key]:` creates the key. Use `if d.get(key):`
  or `key in d` when you only mean to look.
- **Counting indices instead of values** (or the other way round) — decide what the
  dictionary maps to and write it in a comment.
- **Comparing `Counter` objects for a subset relation with `<=`** works in modern Python,
  but was not always available; `all(need[c] <= have[c] for c in need)` is unambiguous.
