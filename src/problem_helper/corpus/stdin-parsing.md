---
id: algo-stdin-parsing
title: Reading stdin in competitive problems
topic: io
level: beginner
tags: input, stdin, parsing, split, int, output format, sys.stdin
summary: Read numbers with split() and map(int, ...), and mind the declared count.
---

# Reading stdin in competitive problems

The judge feeds your program a fixed block of text on standard input and compares standard
output byte for byte. Most "wrong answer" verdicts on an otherwise correct algorithm come
from this layer, not from the algorithm.

## The common layouts

**A count, then that many numbers on one line:**

```python
n = int(input())
values = list(map(int, input().split()))
```

**Several numbers on one line:**

```python
a, b = map(int, input().split())
```

**A count, then that many lines:**

```python
n = int(input())
rows = [input().strip() for _ in range(n)]
```

**Numbers spread over an unknown number of lines** — read everything at once and forget the
line structure:

```python
import sys

data = sys.stdin.read().split()
n = int(data[0])
values = list(map(int, data[1 : 1 + n]))
```

`sys.stdin.read().split()` splits on any whitespace including newlines, so it is immune to
the input being reflowed. It is also the fastest option: `input()` in a loop over 10^5 lines
is measurably slow, and `sys.stdin.readline` (note: keeps the trailing `\n`) is the usual
middle ground.

## Trust the data, not the declared count

`n` tells you how many numbers *should* follow. When the two disagree, the statement is
usually right and your parsing is wrong — but iterating with `for i in range(n)` over a
shorter list raises `IndexError`, while iterating over the list itself simply works. Prefer

```python
for value in values:
    ...
```

over `for i in range(n): values[i]` unless the index itself is needed.

## Reading until EOF

When no count is given:

```python
import sys

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    ...
```

`input()` raises `EOFError` at the end of input, so the `for line in sys.stdin` form is
cleaner than a `while True` with a `try`.

## Output format

- `print(*values)` prints the elements separated by spaces — this is what
  "print the array" means. `print(values)` prints `[1, 2, 3]` and fails.
- `print` adds a newline already; an extra `"\n"` produces a blank line.
- Many prints in a loop are slow. Build the lines and emit them in one call:
  `sys.stdout.write("\n".join(map(str, answers)) + "\n")`.
- Floating point output almost always needs an explicit format: `print(f"{x:.6f}")`.
  The default `repr` may use scientific notation, which the checker will not accept.
- Booleans usually have to be printed as the statement spells them: `"YES"`/`"NO"`, not
  `True`/`False`.

## Multiple test cases in one file

Most contest inputs start with the number of test cases. Wrap the whole solution in a
function and call it in a loop — keeping per-case state inside the function is what prevents
the second case from inheriting the first one's variables.

```python
def solve(data: list[str], pos: int) -> tuple[str, int]:
    ...            # returns the answer and the new read position

data = sys.stdin.read().split()
t, pos = int(data[0]), 1
out = []
for _ in range(t):
    answer, pos = solve(data, pos)
    out.append(answer)
print("\n".join(out))
```

## Pitfalls

- **Trusting `n` instead of `len(values)`** when they disagree.
- **`int(input().split())`** — `split` returns a list; it needs `map(int, ...)`.
- **Leaving `\n` on a value read with `readline`** and comparing it to a clean string.
- **`print(values)`** instead of `print(*values)`.
- **`input()` in a hot loop** on large inputs.
- **Reading the count and then forgetting the rest of that line**, so the next `input()`
  returns an empty string.
- **Extra debug prints** left in the submission — the checker sees them.
