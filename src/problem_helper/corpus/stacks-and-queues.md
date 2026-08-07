---
id: algo-stacks-queues
title: Stacks, queues and monotonic structures
topic: stacks_queues
level: intermediate
tags: stack, queue, deque, LIFO, FIFO, brackets, monotonic stack, next greater
summary: A list is a stack, a deque is a queue, and a monotonic stack answers "next greater" in O(n).
---

# Stacks, queues and monotonic structures

A stack is last-in-first-out (**LIFO**); a queue is first-in-first-out (**FIFO**). Both are
one-line data structures in Python, and the interesting part is recognising which problems
are secretly one of them.

## Stack: a plain list

```python
stack = []
stack.append(x)        # push, amortised O(1)
top = stack[-1]        # peek, O(1) — raises IndexError when empty
x = stack.pop()        # pop, O(1)
if not stack:          # the idiomatic emptiness test
    ...
```

Bracket matching is the canonical example. Push every opening bracket, and on a closing one
check that the top matches:

```python
PAIRS = {")": "(", "]": "[", "}": "{"}

def balanced(s: str) -> bool:
    stack = []
    for ch in s:
        if ch in "([{":
            stack.append(ch)
        elif ch in PAIRS:
            if not stack or stack.pop() != PAIRS[ch]:
                return False
    return not stack        # anything left open means unbalanced
```

The final `return not stack` is the half everybody forgets: `"((("` never fails a check
inside the loop.

## Queue: `collections.deque`, never a list

```python
from collections import deque

queue = deque()
queue.append(x)         # enqueue at the right, O(1)
x = queue.popleft()     # dequeue from the left, O(1)
```

`list.pop(0)` is O(n) because every remaining element shifts down one slot. Inside a BFS
over 10^5 nodes that single character turns a linear algorithm into a quadratic one, and it
is the most common reason a correct BFS times out.

`deque` is double-ended: `appendleft` and `pop` work too, and `deque(maxlen=k)` keeps a
sliding window of the last `k` items automatically.

## Monotonic stack

A stack whose contents stay sorted answers "the next element greater than this one" for
every position in a single O(n) pass. Each index is pushed once and popped once.

```python
def next_greater(a: list[int]) -> list[int]:
    result = [-1] * len(a)
    stack = []                       # indices, values decreasing from bottom to top
    for i, value in enumerate(a):
        while stack and a[stack[-1]] < value:
            result[stack.pop()] = value      # `value` is the next greater for that index
        stack.append(i)
    return result
```

Store **indices**, not values: the answer usually needs a distance ("how many days until a
warmer one"), and an index gives you both. The same skeleton, with the comparison flipped,
gives the previous smaller element, and with a little bookkeeping it computes the largest
rectangle in a histogram and the sum of subarray minimums.

## Monotonic deque: sliding-window maximum

The maximum of every window of size `k`, in O(n) total:

```python
from collections import deque

def window_max(a: list[int], k: int) -> list[int]:
    dq = deque()            # indices, their values decreasing
    out = []
    for i, value in enumerate(a):
        while dq and a[dq[-1]] <= value:
            dq.pop()                     # smaller values can never be the max again
        dq.append(i)
        if dq[0] <= i - k:
            dq.popleft()                 # the front fell out of the window
        if i >= k - 1:
            out.append(a[dq[0]])
    return out
```

The front of the deque is always the maximum of the current window. This is the structure
that rescues sliding-window problems where the aggregate cannot be undone by subtraction.

## Recursion is a stack

Every recursive traversal can be rewritten with an explicit stack, and that is the standard
fix for `RecursionError` on deep inputs. The iterative DFS below visits the same nodes as
the recursive one:

```python
def dfs(graph, start):
    stack, seen = [start], {start}
    while stack:
        node = stack.pop()
        for nxt in graph[node]:
            if nxt not in seen:
                seen.add(nxt)
                stack.append(nxt)
```

Swapping `stack.pop()` for `queue.popleft()` turns exactly this code into a BFS — the only
difference between the two traversals is the container.

## Pitfalls

- **`list.pop(0)` as a dequeue.** Use `deque.popleft`.
- **Peeking or popping an empty stack.** Guard with `if stack` — `IndexError` is the symptom.
- **Forgetting the final emptiness check** in bracket problems.
- **Storing values instead of indices** in a monotonic stack, then being unable to compute
  distances.
- **`<` versus `<=` in the monotonic loop.** It decides how equal elements are treated, and
  it is the difference between counting duplicates once and counting them many times.
- **`deque` has no O(1) indexing.** `dq[i]` for a middle `i` is O(n); only the two ends are
  cheap.
