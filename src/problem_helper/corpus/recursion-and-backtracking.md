---
id: algo-recursion
title: Recursion and backtracking
topic: recursion
level: intermediate
tags: recursion, base case, backtracking, permutations, subsets, recursion limit, pruning
summary: A base case plus a strictly smaller call; backtracking undoes the choice on the way out.
---

# Recursion and backtracking

A recursive function solves a problem by calling itself on a strictly smaller input. Two
things make it correct: a **base case** that returns without recursing, and a guarantee that
every call moves towards that base case. Missing either gives infinite recursion, which
Python reports as `RecursionError: maximum recursion depth exceeded`.

## Shape of a recursive solution

```python
def solve(state):
    if is_base(state):          # 1. stop
        return base_value
    for choice in options(state):
        result = solve(apply(state, choice))    # 2. strictly smaller
        ...                                     # 3. combine
    return combined
```

Trust the recursive call. The most common way to write a broken recursion is to try to
reason about the whole call tree at once; assume `solve` already works for smaller inputs
and only get the current level right.

## The recursion limit

Python's default limit is 1000 frames and each frame is a real stack frame, so deep
recursion is expensive as well as bounded.

```python
import sys
sys.setrecursionlimit(300_000)
```

Raising the limit works up to the point where the C stack itself overflows and the process
dies without a traceback. For a linear chain of 10^5 nodes, an explicit stack is safer than
a bigger limit. Python has no tail-call optimisation, so a "tail recursive" helper still
consumes a frame per call.

## Backtracking

Backtracking is recursion over a search tree where a choice is made, explored, then
**undone**:

```python
def permutations(a: list[int]) -> list[list[int]]:
    result, current, used = [], [], [False] * len(a)

    def walk():
        if len(current) == len(a):
            result.append(current.copy())      # copy — `current` keeps mutating
            return
        for i, value in enumerate(a):
            if used[i]:
                continue
            used[i] = True
            current.append(value)
            walk()
            current.pop()                      # undo
            used[i] = False                    # undo
    walk()
    return result
```

Two lines carry the pattern. `current.copy()` at the base case: appending `current` itself
stores a reference to a list that is about to change, and the result ends up as `n!` copies
of the empty list. And the paired undo after the recursive call: every mutation before
`walk()` needs its mirror image after it, or the state leaks into sibling branches.

Subsets follow the same shape with a binary choice per element (take it or skip it),
combinations add a start index so earlier elements are never revisited, and N-queens adds
constraint checks before descending.

## Pruning

Backtracking is exponential by nature; pruning is what makes it finish. Cut a branch as soon
as it cannot lead to a valid or better answer:

```python
if current_sum + remaining_max < best_found:
    return              # this branch can never win
```

Sorting the candidates first often makes pruning much more effective, because the promising
branches are explored while the bound is still loose. Skipping duplicate values at the same
depth (`if i > start and a[i] == a[i - 1]: continue` on a sorted list) is what keeps
"subsets with duplicates" from reporting the same set many times.

## Recursion versus iteration

Any recursion can be rewritten with an explicit stack, and some are much clearer that way.
Conversely, some iterative code is much clearer as recursion — tree traversals in
particular. Choose by readability, then switch if the depth is a problem.

When the same subproblem is reached along many paths, plain recursion is exponential and the
fix is memoisation — see dynamic programming. Naive `fib(n)` making 2^n calls is the
textbook example, and a single `@cache` decorator makes it linear.

## Pitfalls

- **No base case, or one that is unreachable** for some inputs (negative arguments, empty
  lists).
- **Appending the mutable working list** instead of a copy.
- **Forgetting to undo a choice** after the recursive call.
- **Mutable default arguments.** `def walk(path=[])` shares one list across every top-level
  call; use `None` and create inside.
- **Rebuilding large structures per call.** Passing `a[1:]` copies the list at every level
  and turns O(n) into O(n^2); pass an index instead.
- **Relying on recursion for depths above a few thousand.**
- **`return` missing on the recursive branch.** The function computes the answer and
  discards it, returning `None`.
