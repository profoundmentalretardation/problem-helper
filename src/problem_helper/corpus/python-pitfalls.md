---
id: algo-python-pitfalls
title: Python traps that look like algorithm bugs
topic: language
level: beginner
tags: mutable default, integer division, float precision, copy, aliasing, is, truthiness
summary: Aliasing, integer division and float equality produce wrong answers with no error.
---

# Python traps that look like algorithm bugs

These are the language-level mistakes that make a correct algorithm produce a wrong answer.
None of them raises an exception, which is exactly why they cost so much time: the code
runs, the samples pass, one hidden test fails.

## `[[0] * m] * n` shares rows

```python
grid = [[0] * 3] * 2       # WRONG: two references to ONE row
grid[0][0] = 1
print(grid)                # [[1, 0, 0], [1, 0, 0]]

grid = [[0] * 3 for _ in range(2)]     # right: two independent rows
```

The outer `*` copies the reference, not the list. Every 2D DP table, adjacency matrix and
grid built the first way is silently broken. The same applies to `[[]] * n` and to
`dict.fromkeys(keys, [])`.

## Mutable default arguments

```python
def collect(item, into=[]):     # the list is created ONCE, at definition
    into.append(item)
    return into

collect(1)      # [1]
collect(2)      # [1, 2]  — the previous call's list

def collect(item, into=None):   # correct
    if into is None:
        into = []
```

The default object is shared by every call that does not pass the argument. Inside a
recursion or across test cases this leaks state between runs.

## Assignment does not copy

```python
b = a               # same list, two names
b = a[:]            # shallow copy: new outer list, same inner objects
b = [row[:] for row in a]        # deep enough for a 2D grid
import copy; b = copy.deepcopy(a)  # everything, slow
```

Sorting or appending through `b` when you meant to keep `a` intact is the usual symptom. For
nested structures the shallow copy is not enough — the rows are still shared.

## Integer versus float division

```python
7 / 2       # 3.5   — always a float, even for exact divisions
7 // 2      # 3     — floor division, integer result
-7 // 2     # -4    — floors towards minus infinity, NOT towards zero
int(-7 / 2) # -3    — truncation, a different answer
```

`/` on large integers loses precision above 2^53, so index arithmetic must use `//`.
Mixing the two is how a mid-point calculation becomes a float and an index raises
`TypeError: list indices must be integers`.

For "round half up" behaviour note that the built-in `round` uses banker's rounding:
`round(0.5)` is `0` and `round(2.5)` is `2`.

## Float comparison

```python
0.1 + 0.2 == 0.3            # False
abs(x - y) < 1e-9           # the comparison to write
```

Accumulating floats in a loop compounds the error. Where the input allows it, stay in
integers — multiply monetary values by 100, compare fractions by cross-multiplication
(`a * d == c * b`) instead of dividing.

## `is` versus `==`

`is` compares identity, `==` compares value. Small integers and short strings are interned,
so `256 is 256` is `True` while `257 is 257` may be `False`. Use `is` only for `None`,
`True` and `False`.

## Truthiness

`0`, `""`, `[]`, `{}`, `None` are all falsy. `if not x:` therefore fires for both "missing"
and "zero", which is wrong whenever 0 is a legitimate value. Write `if x is None:` when that
is what you mean. `if len(a) == 0` and `if not a` are equivalent for lists — the second is
idiomatic — but `if a == []` also allocates.

## Late binding in closures

```python
funcs = [lambda: i for i in range(3)]
[f() for f in funcs]        # [2, 2, 2] — every lambda sees the final i

funcs = [lambda i=i: i for i in range(3)]    # bind at definition time
```

## Chained comparison and operator precedence

`a < b < c` means `a < b and b < c` and does what you expect. But `a == b or c` is
`(a == b) or c`, which is truthy whenever `c` is non-zero — the intended form is
`a in (b, c)`.

## Pitfalls, condensed

- `[[0] * m] * n` for a grid.
- A mutable default argument.
- `b = a` when a copy was meant.
- `/` where `//` belongs, and `-7 // 2 == -4`.
- `==` on floats.
- `is` on numbers or strings.
- `if not x` when `x == 0` is valid data.
- A loop variable captured by a lambda.
