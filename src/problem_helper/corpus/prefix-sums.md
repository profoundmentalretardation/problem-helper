---
id: algo-prefix-sums
title: Prefix sums for range queries
topic: prefix_sums
level: intermediate
tags: sum, range, precomputation, cumulative, difference array
summary: Precompute cumulative sums so any range sum is one subtraction.
---

# Prefix sums for range queries

A prefix sum array turns "what is the sum of `a[l..r]`" from an O(n) loop into a single
subtraction. You pay O(n) once, then every query costs O(1). This is the cheapest
precomputation in competitive programming and it shows up inside dozens of other
algorithms.

## Building the array

```python
def prefix(a: list[int]) -> list[int]:
    pref = [0] * (len(a) + 1)
    for i, value in enumerate(a):
        pref[i + 1] = pref[i] + value
    return pref
```

`pref[i]` is the sum of the first `i` elements, so `pref[0] == 0` and `pref[len(a)]` is the
total. The sum of the inclusive range `a[l..r]` is then

```python
range_sum = pref[r + 1] - pref[l]
```

The extra leading zero is what makes the formula work when `l == 0`. An array of length `n`
instead of `n + 1` forces a special case for the first element, and that special case is
where the bug lives.

The standard library builds the same thing:

```python
from itertools import accumulate

pref = [0, *accumulate(a)]        # identical to the loop above
```

`accumulate` also takes an operator, so `accumulate(a, max)` gives running maxima and
`accumulate(a, operator.mul)` running products.

## Counting instead of summing

Prefix sums over a 0/1 array count occurrences in a range. "How many even numbers are in
`a[l..r]`" becomes a prefix sum over `[1 if x % 2 == 0 else 0 for x in a]`. The same trick
answers "how many elements above the threshold" and "how many opening brackets so far".

A useful variant: replace 0 with -1 and a prefix sum tells you where two categories
balance. The longest subarray with as many zeros as ones is the largest distance between
two equal prefix values, found with a dictionary from prefix value to its first index.

```python
def longest_balanced(a: list[int]) -> int:
    first_seen = {0: -1}
    running = 0
    best = 0
    for i, value in enumerate(a):
        running += 1 if value == 1 else -1
        if running in first_seen:
            best = max(best, i - first_seen[running])
        else:
            first_seen[running] = i     # only the FIRST index, never overwrite
    return best
```

Overwriting `first_seen[running]` on a repeat shortens every later answer — the first
occurrence is the one that maximises the distance.

## Two dimensions

For a grid, `pref[i][j]` is the sum of the rectangle from the origin to `(i, j)`, and a
sub-rectangle is inclusion-exclusion over four corners:

```python
def build_2d(grid: list[list[int]]) -> list[list[int]]:
    rows, cols = len(grid), len(grid[0])
    pref = [[0] * (cols + 1) for _ in range(rows + 1)]
    for i in range(rows):
        for j in range(cols):
            pref[i + 1][j + 1] = (
                grid[i][j] + pref[i][j + 1] + pref[i + 1][j] - pref[i][j]
            )
    return pref

def rect_sum(pref, r1, c1, r2, c2):     # inclusive corners
    return (
        pref[r2 + 1][c2 + 1] - pref[r1][c2 + 1] - pref[r2 + 1][c1] + pref[r1][c1]
    )
```

The `+ pref[i][j]` at the end of the build and the `+ pref[r1][c1]` in the query are the
same correction: the overlapping corner was subtracted twice.

## The difference array

The mirror image of a prefix sum. When many range *updates* are followed by a single read
of the whole array, record only the deltas and integrate once at the end:

```python
def apply_ranges(n: int, updates: list[tuple[int, int, int]]) -> list[int]:
    diff = [0] * (n + 1)
    for l, r, value in updates:         # add `value` to a[l..r]
        diff[l] += value
        diff[r + 1] -= value            # the +1 needs the extra slot
    return list(accumulate(diff[:n]))
```

`m` updates cost O(m + n) instead of O(m * n). This is the standard answer to "add X to a
range, many times, then print the array".

## Pitfalls

- **An array of length `n` instead of `n + 1`.** Then `pref[r + 1]` reads past the end for
  the last range.
- **Mixing inclusive and exclusive bounds.** Decide once whether `r` is included and write
  it in a comment next to the formula.
- **Rebuilding the prefix array after every update.** If the array changes between queries,
  a prefix sum is the wrong structure — use a Fenwick tree.
- **Forgetting `diff[r + 1] -= value`** when the range ends at the last index; the extra
  slot in the difference array exists exactly so this is not a special case.
- **Floating point sums.** Subtracting two large cumulative floats loses precision; keep
  integers where the input allows it.
