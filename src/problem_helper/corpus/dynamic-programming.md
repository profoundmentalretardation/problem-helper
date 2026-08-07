---
id: algo-dynamic-programming
title: Dynamic programming from memoisation to tables
topic: dynamic_programming
level: intermediate
tags: DP, memoisation, lru_cache, knapsack, LIS, subproblems, bottom-up
summary: DP is recursion plus a cache; the state definition is the whole problem.
---

# Dynamic programming from memoisation to tables

Dynamic programming (**DP**) applies when a problem decomposes into overlapping subproblems
with an optimal substructure — the best answer for a state is built from the best answers of
smaller states. The technique is mechanical once the state is defined; defining the state is
the actual work.

## Three questions before any code

1. **What is a state?** The minimal set of values that determines the rest of the answer.
   "Index `i` and remaining capacity `c`", not "the whole list of chosen items".
2. **What is the transition?** How a state's answer is built from smaller states.
3. **What are the base cases?** The states whose answers are known without recursion.

If the state does not summarise everything the future depends on, the DP is wrong no matter
how the code is written. That is the failure behind most "my DP gives the right answer on
the samples and fails on test 7".

## Top-down: recursion plus a cache

```python
from functools import lru_cache

def knapsack(weights: list[int], values: list[int], capacity: int) -> int:
    @lru_cache(maxsize=None)
    def best(i: int, left: int) -> int:
        if i == len(weights) or left == 0:
            return 0
        skip = best(i + 1, left)
        if weights[i] > left:
            return skip
        take = values[i] + best(i + 1, left - weights[i])
        return max(skip, take)

    return best(0, capacity)
```

`functools.cache` (Python 3.9+) is `lru_cache(maxsize=None)` under a shorter name. Both
require every argument to be **hashable**, which is why states are ints and tuples, never
lists. Passing a list is the usual cause of `TypeError: unhashable type: 'list'` in a DP.

Top-down is the easier way to get a first correct solution: write the recursion, add the
decorator, done. Its risks are the recursion limit on deep states and the memory the cache
holds.

## Bottom-up: fill a table

The same recurrence, iterated in an order where every dependency is already computed:

```python
def knapsack_table(weights, values, capacity):
    dp = [0] * (capacity + 1)                  # dp[c] = best value with capacity c
    for w, v in zip(weights, values):
        for c in range(capacity, w - 1, -1):   # DOWNWARD for 0/1 knapsack
            dp[c] = max(dp[c], dp[c - w] + v)
    return dp[capacity]
```

The direction of the inner loop is the whole difference between two classic problems.
Descending `c` means each item is used at most once (0/1 knapsack); ascending `c` lets the
same item be reused (unbounded knapsack / coin change). One character, two different
problems — and no error message either way.

## Classic one-dimensional recurrences

**Longest increasing subsequence (LIS)**, O(n^2):

```python
dp = [1] * len(a)                      # dp[i] = LIS length ending exactly at i
for i in range(len(a)):
    for j in range(i):
        if a[j] < a[i]:
            dp[i] = max(dp[i], dp[j] + 1)
answer = max(dp, default=0)
```

The O(n log n) version keeps the smallest possible tail for each length and finds the
insertion point with `bisect_left` — note that it computes the *length* correctly but the
tails array is not itself a valid subsequence.

**Coin change**, minimum number of coins:

```python
INF = float("inf")
dp = [0] + [INF] * amount
for c in range(1, amount + 1):
    for coin in coins:
        if coin <= c:
            dp[c] = min(dp[c], dp[c - coin] + 1)
return -1 if dp[amount] == INF else dp[amount]
```

**Edit distance** and **longest common subsequence** are the two-dimensional versions:
`dp[i][j]` covers the first `i` characters of one string and the first `j` of the other, and
the table needs a row and a column of base cases for the empty prefixes.

## Rolling the table

When `dp[i]` only depends on `dp[i - 1]`, keep two rows instead of `n`:

```python
prev, cur = [0] * (m + 1), [0] * (m + 1)
for i in range(1, n + 1):
    for j in range(1, m + 1):
        cur[j] = ...
    prev, cur = cur, prev            # swap, then overwrite
```

Memory drops from O(n * m) to O(m). Do it only after the O(n * m) version is correct — the
rolled version is much harder to debug, and forgetting to reset the reused row leaves stale
values behind.

## Pitfalls

- **A state that does not capture everything.** The transition then depends on history the
  state does not carry.
- **Wrong iteration order.** Bottom-up requires every dependency to be filled first;
  ascending versus descending in knapsack is the canonical trap.
- **Unhashable arguments to `lru_cache`.** Convert to tuples.
- **Recursion depth** in top-down DP on large inputs.
- **`[[0] * m] * n`** — this creates `n` references to the *same* row. Use
  `[[0] * m for _ in range(n)]`.
- **Confusing "ending exactly at i" with "over the first i elements".** Both are valid state
  definitions with different recurrences and different answer extraction (`dp[-1]` versus
  `max(dp)`); mixing them is a silent wrong answer.
- **Reaching for DP when greedy is provably correct** — or the other way round, which is
  worse because greedy fails only on the tests you did not write.
