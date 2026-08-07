---
id: algo-two-pointers
title: Two pointers on a sorted array
topic: two_pointers
level: beginner
tags: array, sorted, pair sum, linear scan, opposite ends
summary: Walk two indices towards each other to find a pair in O(n) instead of O(n^2).
---

# Two pointers on a sorted array

The two-pointer technique replaces a nested loop with a single pass. Instead of testing
every pair `(i, j)`, you keep two indices and move exactly one of them per step, using an
ordering property of the data to prove that the pairs you skip could never have been the
answer.

## The opposite-ends pattern

The classic form starts with `left` at the beginning and `right` at the end of a **sorted**
array and looks for a pair with a given sum.

```python
def has_pair_with_sum(a: list[int], target: int) -> bool:
    left, right = 0, len(a) - 1
    while left < right:
        current = a[left] + a[right]
        if current == target:
            return True
        if current < target:
            left += 1      # the smallest partner of a[left] is already too small
        else:
            right -= 1     # the largest partner of a[right] is already too large
    return False
```

Each iteration discards a whole row or column of the pair matrix, so the loop runs at most
`n` times: the algorithm is O(n) after the O(n log n) sort, against O(n^2) for the naive
double loop.

Why the discard is safe: if `a[left] + a[right] < target`, then `a[left]` paired with
anything at or below `right` is even smaller, so no pair involving `a[left]` can reach the
target. Dropping `left` loses nothing. The mirror argument covers the other branch.

## The same-direction pattern

The second form runs both pointers forward. `slow` marks where the next kept element goes,
`fast` scans. This is how you remove duplicates or compact an array in place without
allocating a second list.

```python
def dedupe_sorted(a: list[int]) -> int:
    if not a:
        return 0
    slow = 0
    for fast in range(1, len(a)):
        if a[fast] != a[slow]:
            slow += 1
            a[slow] = a[fast]
    return slow + 1        # length of the deduplicated prefix
```

The invariant is that `a[0..slow]` always holds the answer built so far. Everything after
`slow` is scratch space you are allowed to overwrite.

## Three-pointer variants

For a triple summing to a target, fix the first element with an outer loop and run the
opposite-ends scan on the remainder. That is O(n^2) overall — still far better than the
O(n^3) triple loop, and the standard answer to "3-sum" style problems.

```python
def three_sum_zero(a: list[int]) -> list[tuple[int, int, int]]:
    a.sort()
    found = []
    for i in range(len(a) - 2):
        if i > 0 and a[i] == a[i - 1]:
            continue           # skip duplicate anchors
        left, right = i + 1, len(a) - 1
        while left < right:
            total = a[i] + a[left] + a[right]
            if total == 0:
                found.append((a[i], a[left], a[right]))
                left += 1
                while left < right and a[left] == a[left - 1]:
                    left += 1
            elif total < 0:
                left += 1
            else:
                right -= 1
    return found
```

## Merging two sorted sequences

Two pointers also merge: one index per input, always advance the one pointing at the
smaller element. This is the merge step of merge sort, and it is the reason merging is
O(n + m) rather than O((n + m) log (n + m)).

```python
def merge(a: list[int], b: list[int]) -> list[int]:
    i = j = 0
    out = []
    while i < len(a) and j < len(b):
        if a[i] <= b[j]:
            out.append(a[i])
            i += 1
        else:
            out.append(b[j])
            j += 1
    out.extend(a[i:])
    out.extend(b[j:])
    return out
```

Forgetting the two `extend` calls at the end is the most common bug here: the loop stops as
soon as *one* input is exhausted, and the tail of the other one still has to be copied.

## Pitfalls

- **The array must be sorted.** The opposite-ends argument depends on order. If you sort
  inside the function and the caller needs the original order, sort a copy or remember the
  original indices before sorting.
- **`left < right`, not `left <= right`.** With `<=` the two pointers can meet on the same
  element and it pairs with itself, which reports `x + x == target` as a valid pair.
- **Moving both pointers in one step.** Unless you have just recorded a match, exactly one
  pointer moves per iteration; moving both can jump over the answer.
- **Forgetting to skip duplicates.** In problems that ask for distinct pairs or triples,
  advance past equal neighbours after a hit, otherwise the same answer is reported many
  times.
- **Sorting when order matters.** For "find a subarray" problems the elements must stay
  where they are — that is a sliding window, not a two-pointer scan over sorted data.
