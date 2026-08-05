"""A catalog of ready-made sessions: statement, a broken solution and its tests.

These are what the playground loads and what a reviewer runs the service against without
having to invent a broken program first. Each sample is one *realistic* student mistake —
the kind the fixer has to diagnose and the hint has to point at — and each names the study
material the hint should end up citing, which is the seam an agent-level evaluation will
hang off later.

Every sample carries the reference solution as well. It is never sent to the service; it is
there so `tests/test_samples.py` can assert that the tests are well-formed — the reference
passes all of them and the broken code fails at least one. A sample whose broken code
accidentally passes would quietly become a test of the `already_correct` path instead.
"""

from __future__ import annotations

from pydantic import BaseModel

from .schemas import TestCase


class Sample(BaseModel):
    """One broken solution with everything needed to drive a session."""

    id: str
    title: str
    topic: str
    task: str
    code: str
    solution: str
    tests: list[TestCase]
    mistake: str
    expected_material: str


SAMPLES: list[Sample] = [
    Sample(
        id="even-sum",
        title="Sum of the even numbers",
        topic="basics",
        task=(
            "The first line holds an integer N, the second holds N integers separated by "
            "spaces. Print the sum of the even ones."
        ),
        code=(
            "n = int(input())\n"
            "values = list(map(int, input().split()))\n"
            "total = 0\n"
            "for x in values:\n"
            "    if x % 2 == 1:\n"
            "        total += x\n"
            "print(total)\n"
        ),
        solution=(
            "n = int(input())\n"
            "values = list(map(int, input().split()))\n"
            "print(sum(x for x in values if x % 2 == 0))\n"
        ),
        tests=[
            TestCase(input="5\n1 2 3 4 5\n", expected_output="6"),
            TestCase(input="3\n2 4 6\n", expected_output="12"),
            TestCase(input="4\n1 3 5 7\n", expected_output="0"),
        ],
        mistake="The parity condition is inverted: it selects odd numbers.",
        expected_material="algo-parity-filters",
    ),
    Sample(
        id="sum-of-indices",
        title="Sum of the values above the threshold",
        topic="basics",
        task=(
            "The first line holds N and a threshold T. The second line holds N integers. "
            "Print the sum of the values strictly greater than T."
        ),
        code=(
            "n, t = map(int, input().split())\n"
            "values = list(map(int, input().split()))\n"
            "total = 0\n"
            "for i in range(n):\n"
            "    if values[i] > t:\n"
            "        total += i\n"
            "print(total)\n"
        ),
        solution=(
            "n, t = map(int, input().split())\n"
            "values = list(map(int, input().split()))\n"
            "print(sum(x for x in values if x > t))\n"
        ),
        tests=[
            TestCase(input="5 2\n1 2 3 4 5\n", expected_output="12"),
            TestCase(input="3 10\n1 2 3\n", expected_output="0"),
        ],
        mistake="The loop accumulates the index instead of the value at that index.",
        expected_material="algo-parity-filters",
    ),
    Sample(
        id="range-sums",
        title="Range sums with a prefix array",
        topic="prefix_sums",
        task=(
            "The first line holds N and Q. The second holds N integers. Each of the next Q "
            "lines holds L and R (1-based, inclusive). Print the sum of the range for every "
            "query, one per line."
        ),
        code=(
            "n, q = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "pref = [0] * (n + 1)\n"
            "for i in range(n):\n"
            "    pref[i + 1] = pref[i] + a[i]\n"
            "for _ in range(q):\n"
            "    l, r = map(int, input().split())\n"
            "    print(pref[r] - pref[l])\n"
        ),
        solution=(
            "import sys\n"
            "data = sys.stdin.read().split()\n"
            "n, q = int(data[0]), int(data[1])\n"
            "a = [int(x) for x in data[2 : 2 + n]]\n"
            "pref = [0] * (n + 1)\n"
            "for i in range(n):\n"
            "    pref[i + 1] = pref[i] + a[i]\n"
            "out = []\n"
            "pos = 2 + n\n"
            "for _ in range(q):\n"
            "    l, r = int(data[pos]), int(data[pos + 1])\n"
            "    pos += 2\n"
            "    out.append(str(pref[r] - pref[l - 1]))\n"
            "print('\\n'.join(out))\n"
        ),
        tests=[
            TestCase(input="5 2\n1 2 3 4 5\n1 3\n2 5\n", expected_output="6\n14"),
            TestCase(input="3 1\n10 20 30\n1 1\n", expected_output="10"),
        ],
        mistake=(
            "The query mixes the 1-based inclusive bounds of the statement with the "
            "0-based prefix array: it needs pref[r] - pref[l - 1]."
        ),
        expected_material="algo-prefix-sums",
    ),
    Sample(
        id="adjacent-pairs",
        title="Longest non-decreasing run",
        topic="basics",
        task=(
            "The first line holds N, the second holds N integers. Print the length of the "
            "longest run of consecutive non-decreasing elements."
        ),
        code=(
            "n = int(input())\n"
            "a = list(map(int, input().split()))\n"
            "best = 1\n"
            "current = 1\n"
            "for i in range(n):\n"
            "    if a[i] <= a[i + 1]:\n"
            "        current += 1\n"
            "        best = max(best, current)\n"
            "    else:\n"
            "        current = 1\n"
            "print(best)\n"
        ),
        solution=(
            "n = int(input())\n"
            "a = list(map(int, input().split()))\n"
            "best = current = 1\n"
            "for i in range(n - 1):\n"
            "    current = current + 1 if a[i] <= a[i + 1] else 1\n"
            "    best = max(best, current)\n"
            "print(best)\n"
        ),
        tests=[
            TestCase(input="6\n1 2 2 1 5 6\n", expected_output="3"),
            TestCase(input="4\n5 4 3 2\n", expected_output="1"),
            TestCase(input="1\n7\n", expected_output="1"),
        ],
        mistake="The loop reads a[i + 1] but runs to the last index, so it walks off the end.",
        expected_material="algo-loop-bounds",
    ),
    Sample(
        id="pair-with-sum",
        title="Pair with a given sum",
        topic="two_pointers",
        task=(
            "The first line holds N and the target S. The second holds N integers in "
            "non-decreasing order. Print YES if two DIFFERENT positions sum to S, else NO."
        ),
        code=(
            "n, s = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "left, right = 0, n - 1\n"
            "found = False\n"
            "while left <= right:\n"
            "    total = a[left] + a[right]\n"
            "    if total == s:\n"
            "        found = True\n"
            "        break\n"
            "    if total < s:\n"
            "        left += 1\n"
            "    else:\n"
            "        right -= 1\n"
            "print('YES' if found else 'NO')\n"
        ),
        solution=(
            "n, s = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "left, right = 0, n - 1\n"
            "found = False\n"
            "while left < right:\n"
            "    total = a[left] + a[right]\n"
            "    if total == s:\n"
            "        found = True\n"
            "        break\n"
            "    if total < s:\n"
            "        left += 1\n"
            "    else:\n"
            "        right -= 1\n"
            "print('YES' if found else 'NO')\n"
        ),
        tests=[
            TestCase(input="4 8\n1 3 4 6\n", expected_output="NO"),
            TestCase(input="4 7\n1 3 4 6\n", expected_output="YES"),
            TestCase(input="3 4\n2 5 9\n", expected_output="NO"),
        ],
        mistake=(
            "The loop condition allows left == right, so an element is paired with itself "
            "and 4 + 4 is reported as a valid pair."
        ),
        expected_material="algo-two-pointers",
    ),
    Sample(
        id="bracket-balance",
        title="Balanced brackets",
        topic="stacks_queues",
        task=(
            "One line holds a string of the characters ()[]{}. Print YES if the brackets "
            "are balanced, else NO."
        ),
        code=(
            "s = input().strip()\n"
            "pairs = {')': '(', ']': '[', '}': '{'}\n"
            "stack = []\n"
            "ok = True\n"
            "for ch in s:\n"
            "    if ch in '([{':\n"
            "        stack.append(ch)\n"
            "    elif ch in pairs:\n"
            "        if not stack or stack.pop() != pairs[ch]:\n"
            "            ok = False\n"
            "            break\n"
            "print('YES' if ok else 'NO')\n"
        ),
        solution=(
            "s = input().strip()\n"
            "pairs = {')': '(', ']': '[', '}': '{'}\n"
            "stack = []\n"
            "ok = True\n"
            "for ch in s:\n"
            "    if ch in '([{':\n"
            "        stack.append(ch)\n"
            "    elif ch in pairs:\n"
            "        if not stack or stack.pop() != pairs[ch]:\n"
            "            ok = False\n"
            "            break\n"
            "print('YES' if ok and not stack else 'NO')\n"
        ),
        tests=[
            TestCase(input="([]{})\n", expected_output="YES"),
            TestCase(input="(((\n", expected_output="NO"),
            TestCase(input="(]\n", expected_output="NO"),
        ],
        mistake=(
            "Brackets that are opened and never closed leave the stack non-empty, and the "
            "final check for that is missing."
        ),
        expected_material="algo-stacks-queues",
    ),
    Sample(
        id="binary-search-position",
        title="Insertion position in a sorted array",
        topic="binary_search",
        task=(
            "The first line holds N and X. The second holds N integers in non-decreasing "
            "order. Print the number of elements strictly less than X."
        ),
        code=(
            "n, x = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "low, high = 0, n - 1\n"
            "while low <= high:\n"
            "    mid = (low + high) // 2\n"
            "    if a[mid] < x:\n"
            "        low = mid + 1\n"
            "    else:\n"
            "        high = mid\n"
            "print(low)\n"
        ),
        solution=(
            "n, x = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "low, high = 0, n\n"
            "while low < high:\n"
            "    mid = (low + high) // 2\n"
            "    if a[mid] < x:\n"
            "        low = mid + 1\n"
            "    else:\n"
            "        high = mid\n"
            "print(low)\n"
        ),
        tests=[
            TestCase(input="5 4\n1 2 4 4 7\n", expected_output="2"),
            TestCase(input="3 10\n1 2 3\n", expected_output="3"),
            TestCase(input="3 0\n1 2 3\n", expected_output="0"),
        ],
        mistake=(
            "The closed-interval loop condition low <= high is mixed with the half-open "
            "update high = mid, so the range stops shrinking and the search hangs."
        ),
        expected_material="algo-binary-search",
    ),
    Sample(
        id="top-k-largest",
        title="The k largest values",
        topic="sorting",
        task=(
            "The first line holds N and K. The second holds N integers. Print the K largest "
            "of them in decreasing order, separated by spaces."
        ),
        code=(
            "n, k = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "a.sort()\n"
            "print(*a[:k])\n"
        ),
        solution=(
            "n, k = map(int, input().split())\n"
            "a = list(map(int, input().split()))\n"
            "a.sort(reverse=True)\n"
            "print(*a[:k])\n"
        ),
        tests=[
            TestCase(input="5 2\n3 1 4 1 5\n", expected_output="5 4"),
            TestCase(input="4 4\n7 7 1 2\n", expected_output="7 7 2 1"),
        ],
        mistake=(
            "The array is sorted ascending and the first k are taken, which yields the k "
            "smallest values in the wrong order."
        ),
        expected_material="algo-sorting-key",
    ),
    Sample(
        id="word-frequency",
        title="The most frequent word",
        topic="hash_map",
        task=(
            "One line holds words separated by spaces. Print the most frequent word; on a "
            "tie print the alphabetically smallest of them."
        ),
        code=(
            "words = input().split()\n"
            "counts = {}\n"
            "for w in words:\n"
            "    counts[w] = counts.get(w, 0) + 1\n"
            "best = ''\n"
            "best_count = 0\n"
            "for w in counts:\n"
            "    if counts[w] > best_count:\n"
            "        best = w\n"
            "        best_count = counts[w]\n"
            "print(best)\n"
        ),
        solution=(
            "words = input().split()\n"
            "counts = {}\n"
            "for w in words:\n"
            "    counts[w] = counts.get(w, 0) + 1\n"
            "print(min(counts, key=lambda w: (-counts[w], w)))\n"
        ),
        tests=[
            TestCase(input="pear apple apple pear kiwi\n", expected_output="apple"),
            TestCase(input="b a b a c\n", expected_output="a"),
        ],
        mistake=(
            "Ties are broken by dictionary insertion order rather than alphabetically, "
            "because a strictly-greater comparison keeps whichever word was seen first."
        ),
        expected_material="algo-hash-counting",
    ),
    Sample(
        id="grid-count",
        title="Count the marked cells per row",
        topic="language",
        task=(
            "The first line holds R and C. The next R lines hold C characters each, '.' or "
            "'#'. Print R numbers, one per line: how many '#' each row holds."
        ),
        code=(
            "r, c = map(int, input().split())\n"
            "counts = [0] * r\n"
            "grid = [[0] * c] * r\n"
            "for i in range(r):\n"
            "    row = input().strip()\n"
            "    for j in range(c):\n"
            "        if row[j] == '#':\n"
            "            grid[i][j] = 1\n"
            "for i in range(r):\n"
            "    counts[i] = sum(grid[i])\n"
            "print('\\n'.join(map(str, counts)))\n"
        ),
        solution=(
            "r, c = map(int, input().split())\n"
            "out = []\n"
            "for _ in range(r):\n"
            "    out.append(str(input().strip().count('#')))\n"
            "print('\\n'.join(out))\n"
        ),
        tests=[
            TestCase(input="2 3\n#.#\n...\n", expected_output="2\n0"),
            TestCase(input="3 2\n##\n#.\n..\n", expected_output="2\n1\n0"),
        ],
        mistake=(
            "[[0] * c] * r builds r references to one single row, so every write is visible "
            "in every row and all the counts come out equal."
        ),
        expected_material="algo-python-pitfalls",
    ),
]


def all() -> list[Sample]:
    return list(SAMPLES)


def get(sample_id: str) -> Sample | None:
    return next((s for s in SAMPLES if s.id == sample_id), None)
