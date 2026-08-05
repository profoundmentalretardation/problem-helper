---
id: algo-greedy
title: Greedy algorithms and when they are safe
topic: greedy
level: intermediate
tags: greedy, exchange argument, intervals, scheduling, counterexample
summary: Take the locally best option — correct only when an exchange argument proves it.
---

# Greedy algorithms and when they are safe

A greedy algorithm builds the answer by repeatedly taking the option that looks best right
now and never reconsidering. When it works it is the simplest and fastest solution
available; when it does not, it produces a plausible wrong answer that passes the samples.
The code is never the hard part — the proof is.

## Proving a greedy choice

Two arguments cover almost every correct greedy algorithm:

**Exchange argument.** Take any optimal solution. Show that it can be transformed step by
step into the greedy one without ever getting worse. Since the transformation never loses
value, the greedy solution is optimal too.

**Staying ahead.** Show that after every step the greedy partial solution is at least as
good as any other partial solution of the same size. The final answer then cannot be beaten.

If neither argument goes through, look for a counterexample instead — a small input where
greedy loses. Two or three hand-built cases usually settle it, and finding one saves you
from a wrong submission.

## Interval scheduling: sort by end time

The maximum number of non-overlapping intervals is greedy, and the sort key is the point:

```python
def max_non_overlapping(intervals: list[tuple[int, int]]) -> int:
    intervals.sort(key=lambda pair: pair[1])       # by END, not by start, not by length
    count = 0
    current_end = float("-inf")
    for start, end in intervals:
        if start >= current_end:
            count += 1
            current_end = end
    return count
```

Sorting by end works because finishing earliest leaves the most room for everything after
it — a clean exchange argument. Sorting by start or by duration both fail on small
counterexamples; a single long interval starting first swallows the whole timeline.

The related problems use different keys, and the key *is* the algorithm:

- **Minimum number of arrows / points to stab every interval** — sort by end, shoot at the
  end of the first uncovered interval.
- **Minimum meeting rooms** — sort the start and end times separately, or push end times
  into a min-heap and pop those that finished.
- **Merge overlapping intervals** — sort by *start*, then extend the current interval while
  the next one begins before it ends.

## Coin change: greedy only for some coin systems

Taking the largest coin first is optimal for the canonical systems (1, 2, 5, 10, …), and
wrong in general. With coins `{1, 3, 4}` and an amount of 6, greedy takes 4 + 1 + 1 = three
coins, while the optimum is 3 + 3 = two. The reliable answer is the DP recurrence; greedy is
only correct when the statement guarantees a system where it holds.

This is the standard demonstration that "it worked on the samples" is not evidence.

## Where greedy is provably right

- **Fractional knapsack** — sort by value per unit weight. (The 0/1 version is *not*
  greedy; that is DP.)
- **Huffman coding** — repeatedly merge the two lightest nodes with a heap.
- **Minimum spanning tree** — Kruskal (sort edges, union-find) and Prim (heap) are both
  greedy with real proofs.
- **Assigning the smallest sufficient resource** — sort both sides and walk them with two
  pointers.

Notice how many of them start with a sort. "Greedy" in practice usually means "sort by the
right key, then one linear pass", and choosing that key is where the thinking goes.

## Pitfalls

- **No proof.** If you cannot state why the local choice is safe, assume it is not.
- **Sorting by the wrong key.** Start versus end versus length gives three different
  algorithms, one of which is usually correct.
- **Ties.** Equal keys often need a secondary rule; decide it deliberately rather than
  inheriting whatever the sort happened to do.
- **Greedy on 0/1 knapsack.** Value-per-weight is optimal only when items can be split.
- **Mutating the list while iterating** after the sort.
- **Assuming greedy is faster than DP by enough to matter.** If `n` is small, the DP is fast
  enough and provably correct — take the certainty.
