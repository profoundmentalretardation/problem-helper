---
id: algo-sliding-window
title: Sliding window over a sequence
topic: sliding_window
level: beginner
tags: subarray, substring, window, running sum, contiguous
summary: Maintain a running aggregate over a moving range instead of recomputing it.
---

# Sliding window over a sequence

A sliding window answers questions about **contiguous** subarrays or substrings. The window
is a range `[left, right]`; the right edge always moves forward, the left edge follows when
the window stops satisfying the constraint. Because every index enters and leaves the
window at most once, the whole scan is O(n) even though the code contains two loops.

The key difference from two pointers on sorted data: a sliding window never sorts. The
order of the elements is the problem, so it must be preserved.

## Fixed-size window

When the length is given, the window is a queue of known size: add the element that enters,
subtract the element that leaves.

```python
def max_sum_of_k(a: list[int], k: int) -> int:
    window = sum(a[:k])
    best = window
    for right in range(k, len(a)):
        window += a[right] - a[right - k]   # one in, one out
        best = max(best, window)
    return best
```

The whole point is that `window` is *updated*, never recomputed. Calling `sum(a[i:i + k])`
inside the loop silently turns an O(n) algorithm back into O(n * k).

## Variable-size window

When the length depends on a condition, grow on the right and shrink on the left while the
invariant is violated.

```python
def shortest_subarray_at_least(a: list[int], target: int) -> int:
    left = 0
    total = 0
    best = len(a) + 1
    for right, value in enumerate(a):
        total += value
        while total >= target:              # while, not if
            best = min(best, right - left + 1)
            total -= a[left]
            left += 1
    return best if best <= len(a) else 0
```

`while` matters. After one step to the right the window may need to shrink by several
elements before the invariant holds again; an `if` shrinks by at most one and leaves the
window in an illegal state.

This version assumes non-negative numbers. With negative values the sum is not monotone in
the window length, the shrink condition stops being valid, and the problem needs prefix
sums plus a monotonic deque instead.

## Windows with a counter

For "longest substring with at most K distinct characters" the aggregate is a dictionary of
counts rather than a number.

```python
from collections import Counter

def longest_with_k_distinct(s: str, k: int) -> int:
    counts: Counter[str] = Counter()
    left = 0
    best = 0
    for right, ch in enumerate(s):
        counts[ch] += 1
        while len(counts) > k:
            counts[s[left]] -= 1
            if counts[s[left]] == 0:
                del counts[s[left]]         # or len(counts) never shrinks
            left += 1
        best = max(best, right - left + 1)
    return best
```

Deleting the zero entry is not cosmetic: `len(counts)` is the invariant being tested, and a
key left at count zero keeps the window shrinking forever or blocks it from ever growing.

## Recognising the pattern

A problem is a sliding window when all of these hold:

- the answer is a **contiguous** range, not an arbitrary subset;
- extending the range changes the aggregate by a bounded amount you can compute in O(1);
- the constraint is monotone — if a window is illegal, every larger window is illegal too.

If the third property fails, the window cannot shrink greedily and you need a different
technique.

## Pitfalls

- **Recomputing the aggregate inside the loop.** `sum(...)` or `max(...)` over the current
  window undoes the entire optimisation.
- **Forgetting to subtract the element that leaves.** The window keeps the value of an index
  it no longer covers, and every later answer is too large.
- **`if` where the invariant needs a `while`.**
- **Off-by-one in the window length.** With inclusive bounds the length is
  `right - left + 1`. Writing `right - left` reports every window one element short.
- **Initialising `best` to 0 for a minimisation problem.** Use `len(a) + 1` or
  `float("inf")` and convert at the end, otherwise the answer is always 0.
- **Applying the technique to non-contiguous selections.** "Any k elements" is a sorting or
  heap problem; a window only ever sees a run.
