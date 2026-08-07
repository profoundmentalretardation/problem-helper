---
id: algo-shortest-paths
title: Shortest paths in weighted graphs
topic: graphs
level: intermediate
tags: Dijkstra, weighted, negative edges, Bellman-Ford, 0-1 BFS, relaxation
summary: Dijkstra with a heap for non-negative weights; BFS for unit weights; Bellman-Ford when edges can be negative.
---

# Shortest paths in weighted graphs

Once edges carry weights, BFS stops being correct: the path with the fewest edges is not the
path with the smallest total weight. Which algorithm replaces it depends entirely on the
weights.

| Situation | Algorithm | Cost |
|---|---|---|
| All weights equal (unweighted) | BFS | O(V + E) |
| Weights are 0 or 1 | 0-1 BFS with a deque | O(V + E) |
| Non-negative weights | Dijkstra with a heap | O(E log V) |
| Any weights, negative allowed | Bellman-Ford | O(V * E) |
| All pairs, small graph | Floyd-Warshall | O(V^3) |

## Dijkstra with a priority queue

```python
import heapq

def dijkstra(graph: dict[int, list[tuple[int, int]]], start: int) -> dict[int, int]:
    """graph: node -> [(neighbour, weight), ...]. Returns node -> shortest distance."""
    dist = {start: 0}
    heap = [(0, start)]
    while heap:
        d, node = heapq.heappop(heap)
        if d > dist.get(node, float("inf")):
            continue                       # a stale entry, already improved
        for nxt, weight in graph[node]:
            candidate = d + weight
            if candidate < dist.get(nxt, float("inf")):
                dist[nxt] = candidate
                heapq.heappush(heap, (candidate, nxt))
    return dist
```

Two details carry the whole algorithm:

**The stale check.** `heapq` cannot update a key that is already in the heap, so the usual
implementation pushes an improved distance as a *new* entry and skips outdated ones when
they surface. Dropping the `if d > dist[node]: continue` line does not give a wrong answer,
but it re-expands vertices and the complexity degrades.

**The tuple order.** `(distance, node)`, distance first — the heap compares the first
component. `(node, distance)` builds a perfectly working heap that orders by node id and
returns nonsense.

To recover the path itself, keep `parent[nxt] = node` alongside each improvement and walk
the chain backwards from the goal.

## Why Dijkstra needs non-negative weights

Dijkstra finalises a vertex the moment it is popped, on the argument that no cheaper route
can appear later because every remaining edge only adds weight. A negative edge breaks that
argument, and the algorithm returns a confidently wrong answer with no error. If any edge is
negative, use Bellman-Ford:

```python
def bellman_ford(n: int, edges: list[tuple[int, int, int]], start: int):
    dist = [float("inf")] * n
    dist[start] = 0
    for _ in range(n - 1):                 # a shortest path has at most n-1 edges
        changed = False
        for u, v, w in edges:
            if dist[u] + w < dist[v]:
                dist[v] = dist[u] + w
                changed = True
        if not changed:
            break                          # early exit, common and safe
    for u, v, w in edges:                  # one more pass detects a negative cycle
        if dist[u] + w < dist[v]:
            raise ValueError("negative cycle")
    return dist
```

## 0-1 BFS

When every weight is 0 or 1, a deque replaces the heap: a zero-weight edge goes to the
front, a one-weight edge to the back. The deque stays sorted by construction and the whole
search is linear.

```python
from collections import deque

def zero_one_bfs(graph, start, n):
    dist = [float("inf")] * n
    dist[start] = 0
    dq = deque([start])
    while dq:
        node = dq.popleft()
        for nxt, weight in graph[node]:
            if dist[node] + weight < dist[nxt]:
                dist[nxt] = dist[node] + weight
                if weight == 0:
                    dq.appendleft(nxt)
                else:
                    dq.append(nxt)
    return dist
```

This is the standard answer to grid problems where some moves are free — "minimum number of
walls to break", "minimum direction changes".

## Relaxation is the common idea

All three algorithms do the same operation, `if dist[u] + w < dist[v]: dist[v] = dist[u] + w`,
and differ only in the order they apply it. BFS applies it in queue order and works because
all weights are equal; Dijkstra applies it in increasing distance order; Bellman-Ford applies
it to everything, `n - 1` times, and needs no order at all.

## Pitfalls

- **Dijkstra with negative weights.** Silently wrong.
- **`(node, distance)` in the heap** instead of `(distance, node)`.
- **No stale-entry check**, or worse, a `visited` set that blocks a genuine improvement.
- **`float("inf")` mixed with integer output.** Convert to `-1` (or whatever the statement
  asks) before printing.
- **Assuming the graph is connected.** Unreachable vertices keep their infinite distance and
  the statement usually prescribes a specific value for them.
- **Rebuilding the adjacency list inside the loop.**
- **Using BFS on a weighted graph** because it "looked like a grid".
