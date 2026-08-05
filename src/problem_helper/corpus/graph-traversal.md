---
id: algo-graph-traversal
title: Graph traversal with BFS and DFS
topic: graphs
level: intermediate
tags: BFS, DFS, breadth-first search, depth-first search, adjacency list, visited, grid, connected components
summary: BFS uses a queue and finds shortest paths in unweighted graphs; DFS uses a stack or recursion.
---

# Graph traversal with BFS and DFS

Breadth-first search (**BFS**) and depth-first search (**DFS**) visit every vertex reachable
from a start. They differ in one line — the container the frontier lives in — and that
difference decides what they are good for. BFS explores by distance, so on an unweighted
graph the first time it reaches a vertex it has found a shortest path. DFS dives to the end
of a branch first, which suits cycle detection, topological order and connectivity.

## Representing the graph

An adjacency list is a dict (or list) from vertex to its neighbours. Build it once from the
edge list:

```python
from collections import defaultdict

graph = defaultdict(list)
for u, v in edges:
    graph[u].append(v)
    graph[v].append(u)        # both directions only if the graph is UNDIRECTED
```

Adding the reverse edge for a directed graph is the most common modelling mistake, and it
usually turns an "is there a path" answer from `False` into `True`.

For an implicit graph — a grid, a word ladder, a state machine — do not build anything. The
neighbours are computed on demand:

```python
DIRECTIONS = ((-1, 0), (1, 0), (0, -1), (0, 1))       # 4-connectivity

def neighbours(r, c, rows, cols):
    for dr, dc in DIRECTIONS:
        nr, nc = r + dr, c + dc
        if 0 <= nr < rows and 0 <= nc < cols:
            yield nr, nc
```

## BFS: the queue version

```python
from collections import deque

def shortest_path_len(graph, start, goal) -> int:
    queue = deque([(start, 0)])
    seen = {start}
    while queue:
        node, distance = queue.popleft()
        if node == goal:
            return distance
        for nxt in graph[node]:
            if nxt not in seen:
                seen.add(nxt)                 # mark on PUSH, not on pop
                queue.append((nxt, distance + 1))
    return -1
```

Marking a vertex as seen when it is *enqueued* — not when it is dequeued — is what keeps the
queue linear in the number of vertices. Marking on pop lets the same vertex enter the queue
once per incoming edge, which on a dense graph is the difference between running and timing
out.

`popleft` on a `deque`, never `pop(0)` on a list.

When the number of levels matters more than the distance per node, process the queue one
layer at a time:

```python
while queue:
    for _ in range(len(queue)):        # exactly the current level
        node = queue.popleft()
        ...
    depth += 1
```

Capturing `len(queue)` before the inner loop is essential — the loop appends to the same
queue while iterating.

## DFS: recursive and iterative

```python
def dfs(graph, node, seen):
    seen.add(node)
    for nxt in graph[node]:
        if nxt not in seen:
            dfs(graph, nxt, seen)
```

Python's default recursion limit is 1000, so a path graph of 10^5 vertices raises
`RecursionError`. Either raise the limit (`sys.setrecursionlimit(300000)`) or use the
explicit-stack form, which is the same algorithm with the frame kept by hand:

```python
def dfs_iterative(graph, start):
    stack, seen = [start], set()
    while stack:
        node = stack.pop()
        if node in seen:
            continue
        seen.add(node)
        stack.extend(graph[node])
```

## Connected components and flood fill

Counting islands in a grid is one loop over the cells plus one traversal per unvisited land
cell:

```python
def count_islands(grid: list[list[str]]) -> int:
    rows, cols = len(grid), len(grid[0])
    seen = set()
    count = 0
    for r in range(rows):
        for c in range(cols):
            if grid[r][c] == "1" and (r, c) not in seen:
                count += 1
                stack = [(r, c)]
                seen.add((r, c))
                while stack:
                    cr, cc = stack.pop()
                    for nr, nc in neighbours(cr, cc, rows, cols):
                        if grid[nr][nc] == "1" and (nr, nc) not in seen:
                            seen.add((nr, nc))
                            stack.append((nr, nc))
    return count
```

Each cell is visited once, so the whole thing is O(rows * cols) regardless of how many
components there are.

## Cycle detection and topological order

In a **directed** graph, a cycle is a back edge to a vertex still on the recursion stack —
three colours (unvisited / in progress / done) rather than a single `seen` set. In an
**undirected** graph, a cycle is any edge to a visited vertex that is not the one you came
from, so the parent has to be passed down and skipped.

Kahn's algorithm produces a topological order with BFS over in-degrees: repeatedly take a
vertex with in-degree 0, remove it, and decrement its neighbours. If fewer than `n` vertices
come out, the graph has a cycle.

## Pitfalls

- **No `visited` set.** A graph with a cycle turns the traversal into an infinite loop.
- **Marking visited on pop instead of on push** in BFS.
- **`list.pop(0)`** instead of `deque.popleft`.
- **Adding the reverse edge in a directed graph.**
- **Using DFS for shortest paths.** DFS finds *a* path; only BFS finds the shortest one on
  an unweighted graph, and neither works with weights — that is Dijkstra.
- **Mutating the grid to mark visits and then needing the original values.** Use a separate
  `seen` set unless the problem explicitly allows destroying the input.
- **Recursion depth** on long paths.
