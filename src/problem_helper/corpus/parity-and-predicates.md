---
id: algo-parity-filters
title: Filtering by parity and other predicates
topic: basics
level: beginner
tags: even, odd, modulo, filter, sum, accumulator, comprehension
summary: `x % 2 == 0` selects even numbers; the sign of the remainder bites in other bases.
---

# Filtering by parity and other predicates

"Sum the even numbers", "count the values above the average", "print the elements divisible
by k" — all the same shape: a predicate, a filter, an aggregate. The technique is trivial;
the bugs are in the predicate, the accumulator and what exactly gets accumulated.

## Parity

```python
x % 2 == 0        # even
x % 2 != 0        # odd — safer than `== 1` (see below)
x % 2             # truthy for odd, falsy for even
```

In Python the result of `%` carries the sign of the **divisor**, so `-3 % 2 == 1` and
negative odd numbers still test as odd. In C and Java the same expression gives `-1`, which
is why `x % 2 == 1` is a portability trap; `x % 2 != 0` is correct in both worlds and is the
habit worth keeping.

`x & 1` is the bitwise form. It is not faster in any way that matters in Python, and it
misbehaves on negative numbers in languages with sign-magnitude representations, so prefer
the modulo.

## Filter and aggregate in one expression

```python
total = sum(x for x in values if x % 2 == 0)      # sum of the even ones
count = sum(1 for x in values if x > threshold)   # how many pass
evens = [x for x in values if x % 2 == 0]         # the values themselves
any_even = any(x % 2 == 0 for x in values)        # stops at the first hit
```

A generator expression inside `sum` allocates nothing; the list comprehension builds the
whole list first, which only matters when you need the elements afterwards.

The explicit loop is equally fine and often clearer when the body grows:

```python
total = 0                       # start at 0, not at values[0]
for x in values:
    if x % 2 == 0:
        total += x
```

## Summing the wrong thing

The single most common mistake in this family is accumulating the **index** rather than the
**value**:

```python
for i in range(len(values)):
    if values[i] % 2 == 0:
        total += i              # wrong: adds the position
        total += values[i]      # right: adds the number
```

`for x in values` removes the possibility entirely, and `enumerate` makes the distinction
visible when both are genuinely needed.

## Accumulator initialisation

- **Sum** starts at `0`; **product** starts at `1`.
- **Maximum** starts at `float("-inf")` or the first element — never at `0`, which is wrong
  for all-negative input.
- **Minimum** starts at `float("inf")`.
- **Count** starts at `0`.

`sum(empty)` is `0` and `max(empty)` raises; `max(values, default=0)` covers the empty case
without a branch.

## Divisibility and other predicates

```python
x % k == 0                       # divisible by k
x % 100 // 10                    # the tens digit
str(x) == str(x)[::-1]           # palindromic number
x > 0 and (x & (x - 1)) == 0     # power of two
```

For "divisible by 3 or 5" write the condition once and keep the `or` inside the predicate;
counting both separately and adding double-counts the multiples of 15 — the inclusion-
exclusion mistake in miniature.

## Pitfalls

- **Inverting the condition.** `!= 0` selects odd, `== 0` selects even; read it twice
  against the statement.
- **`x % 2 == 1` on negative numbers** in other languages.
- **Summing indices instead of values.**
- **Starting the accumulator at something other than the identity** for the operation.
- **Filtering after aggregating.** `sum(values) if x % 2 == 0` sums everything; the
  condition belongs inside the comprehension.
- **Modifying the list while filtering it.** Build a new list instead.
- **Counting an element twice** when two predicates overlap.
