---
id: algo-sets
title: Sets and fast membership tests
topic: hash_map
level: beginner
tags: set, membership, in, duplicates, union, intersection, frozenset
summary: `x in set` is O(1) and `x in list` is O(n) — the swap that fixes most timeouts.
---

# Sets and fast membership tests

A `set` is a hash table without values. It answers "have I seen this" in constant time, and
swapping a list for a set is the single most effective fix for a solution that is correct
but too slow.

## The cost that matters

```python
x in some_list      # O(n)  — compares against every element
x in some_set       # O(1)  — one hash lookup, on average
x in some_dict      # O(1)  — checks the keys
x in some_string    # O(n)  — substring search
```

Inside a loop over `n` items, the first line makes the whole program O(n^2). Building the
set costs O(n) once:

```python
lookup = set(reference_values)          # once, before the loop
missing = [x for x in queries if x not in lookup]
```

If the collection changes during the loop, keep adding to the set as you go — `set.add` is
O(1) too.

## Construction and duplicates

```python
unique = set(values)                    # duplicates collapse
count_unique = len(set(values))
has_duplicates = len(set(values)) != len(values)
ordered_unique = list(dict.fromkeys(values))     # unique, ORIGINAL order preserved
```

A set has **no order**. Iterating over one gives an arbitrary (though deterministic within a
run) sequence, so anything printed from a set must be sorted first: `print(*sorted(unique))`.
When both uniqueness and first-seen order are needed, `dict.fromkeys` is the idiom — dicts
keep insertion order, sets do not.

## Set algebra

```python
a | b        # union            — everything in either
a & b        # intersection     — in both
a - b        # difference       — in a, not in b
a ^ b        # symmetric difference — in exactly one
a <= b       # subset
a.isdisjoint(b)                  # no common element, without building the intersection
```

These replace whole loops. "Which required items are missing" is `set(required) - set(have)`;
"do these two lists share anything" is `not set(x).isdisjoint(y)`. Each operation is linear
in the smaller operand, which is far better than the nested loop it replaces.

The method forms (`a.union(b)`, `a.intersection(b)`) accept any iterable, while the operators
require both sides to be sets — `{1, 2} | [3]` raises `TypeError`.

## Mutating versus copying

`a |= b` and `a.update(b)` modify `a` in place; `a | b` returns a new set. `add` inserts one
element, `update` inserts many — `s.add([1, 2])` raises because a list is unhashable, and
`s.update([1, 2])` adds two elements rather than one pair. `discard(x)` removes if present,
`remove(x)` raises `KeyError` when absent.

## Hashability

Set elements must be hashable: numbers, strings, tuples of hashables, `frozenset`. Lists,
dicts and sets themselves cannot be members. To store a group of items as one element, use a
`frozenset`; to store coordinates, use a tuple.

```python
seen.add((row, col))                     # tuple key for a grid cell
groups.add(frozenset(members))           # order-insensitive group key
```

A subtle one: `True == 1` and `hash(True) == hash(1)`, so `{1, True}` has a single element.
Mixing booleans and integers in a set loses data silently.

## When a set is the wrong tool

- The **count** matters → `Counter`.
- The **position** matters → a dict from value to index.
- The **order** matters → a list, or `dict.fromkeys` for ordered uniqueness.
- The **k smallest** matter → a heap.
- Ranges, not individual values → sort the intervals instead.

## Pitfalls

- **`x in list` inside a loop.**
- **Printing a set directly** and expecting sorted order.
- **`{}` is an empty dict**, not an empty set — that is `set()`.
- **Unhashable elements** (`TypeError: unhashable type: 'list'`).
- **`remove` on an absent element**; use `discard`.
- **Rebuilding the set inside the loop**, which pays the O(n) construction every iteration.
- **Losing duplicates you needed.** A set answers "which values", never "how many of each".
