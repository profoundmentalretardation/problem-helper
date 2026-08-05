---
id: algo-strings
title: Working with strings efficiently
topic: strings
level: beginner
tags: string, immutable, join, slicing, palindrome, split, concatenation
summary: Strings are immutable — build with a list and join; slicing copies.
---

# Working with strings efficiently

Python strings are **immutable**. Every operation that looks like a modification builds a
new string, and that fact drives every performance rule below.

## Never concatenate in a loop

```python
# O(n^2): each += copies the whole string built so far
out = ""
for piece in pieces:
    out += piece

# O(n): collect, then join once
parts = []
for piece in pieces:
    parts.append(piece)
out = "".join(parts)
```

For 10^5 pieces the difference is seconds versus milliseconds. `join` is also the right way
to print a list of numbers: `print(" ".join(map(str, values)))` — `print(values)` emits the
list literal with brackets and commas, which no judge accepts.

## Slicing copies

`s[a:b]` builds a new string of length `b - a`. A loop that slices inside is quietly
quadratic:

```python
for i in range(len(s)):
    if s[i:i + len(word)] == word:      # O(len(word)) per position — usually fine
        ...
```

That one is acceptable because the slice is short. `s[i:]` inside a loop is not — it copies
the whole tail every iteration. Prefer indices, `str.find(sub, start)` or `str.startswith(sub, i)`,
which compare in place.

## Palindromes and reversal

```python
s == s[::-1]                                  # simple, allocates one copy
all(s[i] == s[~i] for i in range(len(s) // 2))   # no copy; ~i is -i-1, the mirror index
```

For "is it a palindrome ignoring case and non-letters", normalise first rather than
complicating the comparison:

```python
cleaned = "".join(ch.lower() for ch in s if ch.isalnum())
```

Expanding around each centre is the standard O(n^2) approach to the *longest* palindromic
substring, and it needs `2n - 1` centres — one per character plus one per gap — because
even-length palindromes have no middle character.

## The methods worth knowing

```python
s.split()          # splits on ANY run of whitespace, drops empties — for tokens
s.split(",")       # splits on exactly one comma, keeps empty fields — for CSV-like input
s.strip()          # both ends; .rstrip("\n") when only the newline is unwanted
s.count(sub)       # non-overlapping occurrences
s.find(sub)        # index or -1; .index raises instead
s.replace(a, b)    # all occurrences, returns a new string
s.startswith(x)    # accepts a tuple: s.startswith(("ab", "cd"))
s.isdigit()        # careful: true for "²" and other unicode digits
```

The two `split` behaviours are genuinely different and mixing them up is a classic parsing
bug: `"a,,b".split(",")` gives three fields, `" a  b ".split()` gives two tokens.

`str.translate` with `str.maketrans` replaces many characters in one pass and beats chained
`replace` calls.

## Character arithmetic

```python
ord("a")                    # 97
chr(97)                     # "a"
index = ord(ch) - ord("a")  # 0..25 bucket for lowercase letters
counts = [0] * 26           # a fixed array is faster than a dict for a known alphabet
```

For anagram checks, either a 26-slot array or `collections.Counter` works; the sorted-string
key (`"".join(sorted(word))`) is shorter and O(k log k) per word.

## Encoding and comparison

Comparison is by code point, so `"Z" < "a"` is true and case-insensitive sorting needs
`key=str.lower`. `str.casefold()` is the aggressive variant for non-English text.

Trailing whitespace is invisible and breaks equality. When a judge compares output exactly,
`print` already adds a newline — a manual `"\n"` on top produces a blank line that fails the
test.

## Pitfalls

- **`+=` in a loop.**
- **`print(list)`** instead of joining.
- **Confusing `split()` with `split(" ")`.**
- **Assuming `s[i] = c` works.** It raises `TypeError`; convert to a list of characters,
  mutate, then join.
- **`is` for string comparison.** It compares identity; short literals are interned and it
  appears to work until it does not.
- **Forgetting `strip()`** on lines read from stdin.
- **Slicing the tail repeatedly** inside a loop.
