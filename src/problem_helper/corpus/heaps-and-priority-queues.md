---
id: algo-heaps
title: Heaps and priority queues
topic: heaps
level: intermediate
tags: heapq, priority queue, min-heap, top k, nlargest, median
summary: heapq is a min-heap; negate the key for a max-heap and keep only k items for top-k.
---

# Heaps and priority queues

A binary heap keeps the smallest element reachable in O(1) and supports insertion and
extraction in O(log n). It is the right structure whenever you repeatedly need the current
extreme of a changing collection — and the wrong one when you need the whole order, which
is what sorting is for.

## `heapq` operates on a plain list

Python has no heap class; `heapq` is a set of functions over a list that maintain the heap
invariant.

```python
import heapq

heap = []
heapq.heappush(heap, value)          # O(log n)
smallest = heap[0]                   # peek, O(1), does not remove
smallest = heapq.heappop(heap)       # O(log n)
heapq.heapify(existing_list)         # O(n), in place — cheaper than n pushes
```

`heap[0]` is the minimum, but the rest of the list is **not** sorted. Printing the list or
indexing into `heap[1]` gives heap order, not sorted order. To get everything in order, pop
repeatedly (that is heapsort, O(n log n)).

Two combined operations exist and are worth using: `heappushpop(heap, x)` pushes then pops
in one pass, and `heapreplace(heap, x)` pops then pushes. Both are faster than the two calls
and differ in whether the new element can be the one returned.

## Max-heap by negation

`heapq` only implements a min-heap. For a max-heap, store negated keys and negate again on
the way out:

```python
heapq.heappush(heap, -value)
largest = -heapq.heappop(heap)
```

Forgetting the second negation is the classic bug — the code runs, the answer is the right
magnitude with the wrong sign.

## Top-k without sorting everything

For the `k` largest of `n` items, keep a min-heap of size `k`. The smallest of the kept
items sits at `heap[0]`, so a new candidate only has to beat that one.

```python
def top_k(values: list[int], k: int) -> list[int]:
    heap = []
    for value in values:
        if len(heap) < k:
            heapq.heappush(heap, value)
        elif value > heap[0]:
            heapq.heapreplace(heap, value)     # evict the current smallest
    return sorted(heap, reverse=True)
```

This is O(n log k) and O(k) memory, against O(n log n) and O(n) for sorting everything. It
is also what `heapq.nlargest(k, values)` and `heapq.nsmallest(k, values)` do internally —
both accept `key=`, so reach for them before writing the loop.

## Tuples as priorities

To order by something other than the element itself, push `(priority, item)` tuples. Ties
then compare the second component, which fails if the items are not comparable:

```python
heapq.heappush(heap, (priority, counter, task))    # counter breaks ties, keeps it stable
```

An incrementing counter as the middle field guarantees the third is never compared and
makes the heap stable (FIFO among equal priorities). Without it, `(1, {"a": 1})` versus
`(1, {"b": 2})` raises `TypeError: '<' not supported between instances of 'dict' and 'dict'`
— only sometimes, only when priorities collide, which makes it a nasty intermittent bug.

## Where heaps show up

- **Dijkstra's algorithm** — the frontier is a priority queue of `(distance, node)`.
- **Merging k sorted lists** — a heap of one element per list, O(total log k).
- **Running median** — a max-heap of the lower half and a min-heap of the upper half, kept
  within one element of each other in size.
- **Scheduling and interval problems** — a min-heap of end times answers "how many rooms are
  in use right now".

## Pitfalls

- **Treating the list as sorted.** Only `heap[0]` is meaningful.
- **Forgetting to negate on the way out** of a max-heap.
- **Pushing tuples whose second field is not comparable**, with no tie-breaker.
- **`heappush` in a loop over an existing list** — `heapify` is O(n), `n` pushes are
  O(n log n).
- **Mutating an object already in the heap.** The invariant is not rechecked; the heap is
  silently corrupted. Push a new entry and mark the old one stale instead.
- **Using a heap when you need arbitrary deletion or a lookup by key.** `heapq` has no
  `remove`; use the lazy-deletion pattern (skip stale entries when they surface) or a
  different structure.
