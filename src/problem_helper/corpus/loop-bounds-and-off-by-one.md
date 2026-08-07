---
id: algo-loop-bounds
title: Loop bounds and off-by-one errors
topic: basics
level: beginner
tags: range, off-by-one, index, loop, enumerate, boundary, empty input
summary: `range(n)` stops at n - 1; comparing neighbours needs one iteration less.
---

# Loop bounds and off-by-one errors

An off-by-one error is a correct algorithm applied to a range that is one element too long
or too short. It survives the sample tests more often than any other bug class, because the
samples rarely include the boundary that breaks.

## What `range` actually yields

`range(a, b)` yields `a, a + 1, …, b - 1`. The right end is **excluded**; the count of values
is `b - a`.

```python
range(n)         # 0 .. n-1        — n values, every index of a list of length n
range(1, n + 1)  # 1 .. n          — n values, when the statement counts from 1
range(n - 1)     # 0 .. n-2        — pairs (i, i+1)
range(n, 0, -1)  # n .. 1          — descending, the stop is still excluded
```

Slices follow the same rule, which is why `a[:k] + a[k:]` reconstructs the list with no gap
and no overlap.

## Comparing neighbours

Reading `a[i + 1]` means the loop must stop one step early:

```python
for i in range(len(a) - 1):
    if a[i] > a[i + 1]:
        return False            # not sorted
```

`range(len(a))` here raises `IndexError` on the last step. The mirror version — iterating
from 1 and looking back at `a[i - 1]` — needs `range(1, len(a))` and reads more naturally
when the comparison is with the previous element.

Better still, let the library express the pairing:

```python
for prev, cur in zip(a, a[1:]):
    ...
all(x <= y for x, y in zip(a, a[1:]))       # "is sorted", no indices at all
```

`zip` stops at the shorter argument, so the off-by-one disappears by construction.

## Index or value

`enumerate` gives both and removes the most common reason to index at all:

```python
for i, value in enumerate(a):           # i and a[i], always in step
    ...
for i, value in enumerate(a, start=1):  # when the statement numbers from 1
    ...
```

Writing `for i in range(len(a)): value = a[i]` is not wrong, but every extra index is
another chance to get a bound wrong.

## One-based statements

Problems often number positions from 1 while Python indexes from 0. Convert **once**, at the
boundary where the data is read or the answer is printed, rather than sprinkling `- 1`
through the body:

```python
queries = [(l - 1, r - 1) for l, r in raw_queries]     # convert on read
print(index + 1)                                       # convert on write
```

Half-open ranges are the other half of this: if a query is "the inclusive range l..r", the
prefix-sum formula needs `pref[r + 1] - pref[l]`, and writing `pref[r] - pref[l]` drops the
last element of every query.

## The boundaries that break

Check these before submitting — they are the inputs the samples usually omit:

- **Empty input.** `len(a) - 1` becomes `-1`, `range(-1)` is empty (fine), but `a[0]` and
  `max(a)` both raise. `max(a, default=0)` handles it in place.
- **A single element.** Any loop over pairs runs zero times; the answer must still be
  correct.
- **All elements equal.** Strict comparisons (`<`) never fire; `<=` fires everywhere.
- **The first and last positions.** Sentinel-free code often mishandles exactly these.
- **The largest allowed value.** Check that an accumulator is not initialised to something
  the data can exceed — `best = 0` fails on all-negative input; use `float("-inf")` or
  `a[0]`.

## Pitfalls

- **`range(len(a))` when reading `a[i + 1]`.**
- **`range(1, n)` when the first element matters** — that skips index 0.
- **Empty input making `len(a) - 1` negative** and a slice silently empty rather than an
  error.
- **Inclusive versus exclusive** mixed inside one formula.
- **Mutating a list while iterating over it by index.** Removals shift everything down and
  the loop skips elements; iterate over a copy or build a new list.
- **`while i < n` with the increment inside an `if`.** When the branch is skipped, the loop
  never terminates.
