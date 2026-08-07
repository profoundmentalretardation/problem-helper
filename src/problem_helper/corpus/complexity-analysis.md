---
id: algo-complexity
title: Estimating time complexity
topic: complexity
level: intermediate
tags: big-o, performance, timeout, nested loops, TLE, amortised
summary: Count the nested loops before optimising: 1e8 simple steps is the practical ceiling.
---

# Estimating time complexity

Complexity analysis answers one practical question: will this run inside the time limit? The
estimate takes seconds and saves you from writing the wrong algorithm.

## The budget

A judge typically allows one to two seconds. Python executes roughly 10^7 simple operations
per second — an order of magnitude below C++. Read the constraints, compute the step count,
compare:

| n | Affordable complexity |
|---|---|
| n ≤ 10 | O(n!) — brute-force permutations |
| n ≤ 20 | O(2^n) — subsets, bitmask DP |
| n ≤ 500 | O(n^3) — Floyd-Warshall, triple loops |
| n ≤ 5 000 | O(n^2) — pairwise DP |
| n ≤ 10^5 | O(n log n) — sorting, heaps, binary search |
| n ≤ 10^7 | O(n) — a single pass, careful with constants |

At n = 10^5 an O(n^2) solution is 10^10 steps: not "slow", but hours. No amount of
micro-optimisation rescues the wrong complexity class, and that is the point of estimating
first.

## Reading the code

Nested loops over the same `n` multiply: two levels is O(n^2), three is O(n^3). Sequential
loops add, and O(n) + O(n log n) is O(n log n) — only the dominant term survives. A loop
that halves the range each step is O(log n), and a loop *containing* one is O(n log n).

Recursion follows the recursion tree: two calls per level with depth `n` is O(2^n); two
calls on half the input is O(n log n) by the master theorem. Memoisation replaces the tree
with the number of distinct states, which is why DP is usually "states × transition cost".

## Hidden loops

The operations that look O(1) and are not are where most surprises live:

| Expression | Cost |
|---|---|
| `x in some_list` | O(n) — scans |
| `x in some_set` / `x in some_dict` | O(1) average |
| `list.insert(0, x)` / `list.pop(0)` | O(n) — shifts everything |
| `deque.appendleft` / `popleft` | O(1) |
| `s += piece` on a string | O(len(s)) — copies |
| `a[1:]` | O(n) — copies |
| `min(a)` / `max(a)` / `sum(a)` | O(n) |
| `sorted(a)` | O(n log n) |
| `list.remove(x)` | O(n) |
| `heapq.heappush/heappop` | O(log n) |
| `dict` / `set` construction from n items | O(n) |

Any of these inside a loop over `n` adds a factor of `n`. The classic timeout is `if x in
seen_list` inside the main loop: correct, readable, and quadratic. Changing `seen_list = []`
to `seen = set()` is a one-character-per-line fix that changes the complexity class.

## Amortised cost

`list.append` is O(1) *amortised*: occasionally the list reallocates and copies everything,
but spread over all appends the average is constant. The same reasoning explains why a
sliding window with a nested `while` is still O(n) — each index enters and leaves the window
once, so the inner loop runs `n` times *in total*, not `n` times per outer step. Counting a
nested `while` as an automatic O(n^2) overestimates a whole family of correct algorithms.

## Space matters too

A list of 10^7 Python ints costs hundreds of megabytes because every element is a boxed
object. `array`, `bytearray` or a rolled DP table are the usual fixes. Memory limits are
usually 256 MB and are enforced just as strictly as the time limit.

## Typical rewrites

- O(n^2) pair search → hash map, O(n).
- O(n^2) pair search on sorted data → two pointers, O(n log n) with the sort.
- Repeated range sums → prefix sums, O(1) per query after O(n).
- Repeated "is it there" over a list → set, O(1) per query.
- Repeated minimum of a changing collection → heap, O(log n) per operation.
- String building in a loop → collect and `join`.

## Pitfalls

- **Optimising constants instead of the class.** Rewriting a loop to a comprehension does
  not save an O(n^2) algorithm.
- **Ignoring the cost of the built-ins** listed above.
- **Assuming a nested `while` implies O(n^2)** without checking the amortised argument.
- **Forgetting the sort** you added at the top when reporting the complexity.
- **Estimating with the sample input** rather than the worst case in the constraints.
