---
id: algo-binary-search
title: Binary search and its boundaries
topic: binary_search
level: beginner
tags: search, sorted, off-by-one, logarithmic, bisect, lower bound
summary: Halve the search range each step; most bugs live in the boundary update.
---

# Binary search and its boundaries

Binary search finds a value in a sorted sequence in O(log n) by discarding half the range
per comparison. The algorithm is four lines long and almost every implementation bug is in
the same place: the relationship between the loop condition and the boundary update.

## The two consistent forms

**Closed interval**, `[low, high]`, both ends are candidates:

```python
def find(a: list[int], target: int) -> int:
    low, high = 0, len(a) - 1
    while low <= high:                 # <= because low == high is still a candidate
        mid = (low + high) // 2
        if a[mid] == target:
            return mid
        if a[mid] < target:
            low = mid + 1              # mid is excluded, it was tested
        else:
            high = mid - 1
    return -1
```

**Half-open interval**, `[low, high)`, the right end is past the last candidate:

```python
def lower_bound(a: list[int], target: int) -> int:
    low, high = 0, len(a)              # note: len(a), not len(a) - 1
    while low < high:                  # < because high is not a candidate
        mid = (low + high) // 2
        if a[mid] < target:
            low = mid + 1
        else:
            high = mid                 # mid stays in the range
    return low                         # first index with a[i] >= target
```

Pick one form and stay inside it. Mixing them — `low <= high` with `high = mid`, or
`low < high` with `high = mid - 1` — produces either an infinite loop or a search that
skips the answer. That single inconsistency is the most common binary-search bug there is.

## Insertion points instead of membership

`lower_bound` above returns the position where `target` would be inserted to keep the array
sorted, which answers far more questions than "is it there":

- `lower_bound(a, x)` — the number of elements strictly less than `x`;
- `upper_bound(a, x)` — the first index with `a[i] > x`;
- `upper_bound(a, x) - lower_bound(a, x)` — how many times `x` occurs.

The standard library ships both. Use them instead of rewriting the loop:

```python
import bisect

i = bisect.bisect_left(a, x)    # lower bound: first index with a[i] >= x
j = bisect.bisect_right(a, x)   # upper bound: first index with a[i] > x
count_of_x = j - i
bisect.insort(a, x)             # insert while keeping the list sorted, O(n) for the shift
```

`bisect_left` and `bisect_right` differ only in how they treat equal elements, and choosing
the wrong one is the usual source of an answer that is off by exactly the multiplicity of
the target. Both accept `lo` and `hi` arguments to search a slice without copying it.

## Binary search over the answer

The array does not have to exist. Whenever a predicate is monotone — false, false, …, true,
true — you can binary search the *answer space*:

```python
def min_capacity(weights: list[int], days: int) -> int:
    def feasible(capacity: int) -> bool:
        used, load = 1, 0
        for w in weights:
            if load + w > capacity:
                used += 1
                load = 0
            load += w
        return used <= days

    low, high = max(weights), sum(weights)
    while low < high:
        mid = (low + high) // 2
        if feasible(mid):
            high = mid
        else:
            low = mid + 1
    return low
```

This is the standard answer to "minimise the maximum" and "maximise the minimum" problems.
The cost is O(log(range) * cost of the check), and the hard part is proving the predicate is
monotone — if a smaller capacity can be feasible when a larger one is not, the search is
meaningless.

For a real-valued answer, iterate a fixed number of times (100 iterations of halving is far
below any tolerance a judge asks for) instead of comparing floats for equality.

## Pitfalls

- **Computing `mid` outside the loop.** It has to be recomputed from the current bounds
  every iteration.
- **`high = mid` in the closed form.** When `low` and `high` are adjacent, `mid` equals
  `low`, the range never shrinks, and the loop hangs.
- **Forgetting the array is not sorted.** Binary search on unsorted input returns
  plausible-looking nonsense, not an error.
- **Returning `mid` when the task asks for an insertion point.** The insertion point is
  `low` after the loop, not the last `mid` you looked at.
- **Overflow-style `(low + high) // 2`** is safe in Python — integers are arbitrary
  precision — but the habit of writing `low + (high - low) // 2` costs nothing and carries
  over to other languages.
- **Searching a list you keep mutating.** `insort` shifts elements; positions captured
  before the insert are stale afterwards.
