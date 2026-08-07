---
id: algo-number-theory
title: Number theory basics
topic: number_theory
level: intermediate
tags: GCD, LCM, modulo, primes, sieve, divisors, fast power, Euclid
summary: GCD by Euclid, primes by sieve, and modular arithmetic that keeps numbers small.
---

# Number theory basics

A handful of number-theory routines cover most of what competitive problems need. All of
them are short, and all of them have a naive version that is too slow by a factor of the
input size.

## GCD and LCM

The greatest common divisor (**GCD**) comes from Euclid's algorithm, which is O(log min(a, b)):

```python
from math import gcd, lcm

g = gcd(a, b)
l = lcm(a, b)          # Python 3.9+
```

Write it by hand only if you need the extended version. The identity behind `lcm` is
`a * b // gcd(a, b)` — divide **before** multiplying (`a // gcd(a, b) * b`) when the numbers
are large in a language with fixed-width integers; in Python it only affects speed.

`gcd` accepts any number of arguments, so `gcd(*values)` reduces a whole list. `gcd(0, x)`
is `x`, which makes 0 the right accumulator to start from.

## Primality and factorisation

Testing one number: trial division up to the square root, because a composite `n` always has
a factor at or below `sqrt(n)`.

```python
def is_prime(n: int) -> bool:
    if n < 2:
        return False
    if n % 2 == 0:
        return n == 2
    f = 3
    while f * f <= n:            # f * f <= n, not f <= n ** 0.5 (float precision)
        if n % f == 0:
            return False
        f += 2
    return True
```

`f * f <= n` keeps everything in integers; `f <= sqrt(n)` introduces a float and, at the
boundary for large `n`, occasionally the wrong answer.

Factorisation is the same loop, dividing out each factor as it is found, and whatever is
left above 1 at the end is itself a prime factor:

```python
def factorise(n: int) -> dict[int, int]:
    factors: dict[int, int] = {}
    f = 2
    while f * f <= n:
        while n % f == 0:
            factors[f] = factors.get(f, 0) + 1
            n //= f
        f += 1
    if n > 1:
        factors[n] = factors.get(n, 0) + 1     # the leftover prime — easy to forget
    return factors
```

## Sieve of Eratosthenes

For "all primes up to N" or many primality queries, the sieve is O(N log log N) and trial
division per number is not viable:

```python
def sieve(n: int) -> list[int]:
    is_composite = bytearray(n + 1)
    is_composite[0:2] = b"\x01\x01"
    for p in range(2, int(n ** 0.5) + 1):
        if not is_composite[p]:
            is_composite[p * p :: p] = b"\x01" * len(is_composite[p * p :: p])
    return [i for i, comp in enumerate(is_composite) if not comp]
```

Starting the inner marking at `p * p` rather than `2 * p` is the standard optimisation:
every smaller multiple already has a smaller prime factor and was crossed out earlier.

A smallest-prime-factor sieve (store the factor instead of a flag) additionally gives O(log n)
factorisation for every number in the range.

## Modular arithmetic

Problems that ask for an answer "modulo 10^9 + 7" want the modulus applied at every step,
not once at the end — the intermediate value would otherwise be astronomically large and, in
most languages, overflow.

```python
MOD = 10 ** 9 + 7
total = (total + term) % MOD
product = product * factor % MOD
```

Addition, subtraction and multiplication distribute over the modulus. **Division does not.**
To divide by `x` modulo a prime `p`, multiply by the modular inverse:

```python
inv = pow(x, MOD - 2, MOD)       # Fermat's little theorem, MOD must be prime
inv = pow(x, -1, MOD)            # Python 3.8+, works for any coprime x
```

`pow(base, exponent, modulus)` is the built-in fast exponentiation: O(log exponent) and it
never builds the huge intermediate. `base ** exponent % modulus` computes the full power
first and can hang on large exponents.

Python's `%` always returns a result with the sign of the divisor, so `-7 % 3 == 2` — no
manual `+ MOD` correction is needed after a subtraction, unlike in C or Java.

## Pitfalls

- **`f <= n ** 0.5`** instead of `f * f <= n`.
- **Forgetting the leftover prime factor** above the loop bound.
- **Trial division per query** where a sieve is called for.
- **`x ** y % m`** instead of `pow(x, y, m)`.
- **Dividing under a modulus** without an inverse.
- **`1` counted as prime**, or 2 excluded by an odd-only loop that never special-cases it.
- **Applying the modulus only to the final answer.**
