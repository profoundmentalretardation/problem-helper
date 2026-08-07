"""The code shield, on both sides: what it must catch and what it must not.

The second half matters more than the first. A shield that refuses `import socket` and also
refuses a legitimate fast-input idiom has traded a problem the container already solves for
a student who cannot get a hint, and the false-positive rate in the README is measured on
exactly this kind of code.
"""

import pytest

from problem_helper import codeshield

# What a student actually writes. None of it may be refused.
LEGITIMATE = [
    "a, b = map(int, input().split())\nprint(a + b)\n",
    "import sys\ndata = sys.stdin.read().split()\nprint(len(data))\n",
    "import sys\nfor line in sys.stdin:\n    print(line.strip())\n",
    # `open(0)` and `os.read(0, ...)` are the two standard fast-input idioms.
    "data = open(0).read().split()\nprint(sum(map(int, data)))\n",
    "import os\ndata = os.read(0, 10 ** 7).split()\nprint(len(data))\n",
    "from collections import Counter\nprint(Counter(input().split()).most_common(1))\n",
    "import bisect\na = [1, 3, 5]\nprint(bisect.bisect_left(a, 4))\n",
    "import heapq\nh = []\nheapq.heappush(h, 3)\nprint(heapq.heappop(h))\n",
    "import math\nprint(math.gcd(12, 18))\n",
    "import json\nprint(json.dumps({'a': 1}))\n",
    "class Node:\n    def __init__(self, v):\n        self.v = v\nprint(Node(1).v)\n",
    "import itertools\nprint(list(itertools.permutations([1, 2])))\n",
    "print(getattr(complex(1, 2), 'real'))\n",
    # A recursive solution that raises its own limit — common, and harmless.
    "import sys\nsys.setrecursionlimit(10000)\n\n\ndef f(n):\n    return 1 if n < 2 else f(n - 1)\n\n\nprint(f(5))\n",
]

HOSTILE = [
    ("import socket\ns = socket.create_connection(('x', 80))\n", "denied-import"),
    ("import subprocess\nsubprocess.run(['ls'])\n", "denied-import"),
    ("from urllib.request import urlopen\nurlopen('http://x')\n", "denied-import"),
    ("import os\nprint(os.environ)\n", "os-attribute"),
    ("import os\nos.system('id')\n", "os-attribute"),
    ("from os import system\nsystem('id')\n", "os-attribute"),
    ("print(open('/etc/passwd').read())\n", "file-access"),
    ("exec(input())\n", "denied-builtin"),
    ("eval('1 + 1')\n", "denied-builtin"),
    ("print(__import__('socket'))\n", "denied-builtin"),
    ("print(().__class__.__bases__[0].__subclasses__())\n", "escape-dunder"),
    ("f = lambda: 0\nprint(f.__globals__)\n", "escape-dunder"),
    ("import os\nprint(getattr(os, 'sys' + 'tem'))\n", "dynamic-attribute"),
    ("import ctypes\n", "denied-import"),
    ("import pickle\npickle.loads(b'')\n", "denied-import"),
]


@pytest.mark.parametrize("code", LEGITIMATE)
def test_legitimate_code_passes(code):
    verdict = codeshield.scan(code)

    assert verdict.allowed, verdict.findings


@pytest.mark.parametrize(("code", "rule"), HOSTILE)
def test_hostile_code_is_refused(code, rule):
    verdict = codeshield.scan(code)

    assert not verdict.allowed
    assert rule in {finding.rule for finding in verdict.findings}


def test_broken_code_is_allowed_through():
    """A SyntaxError is the service's main use case; it also cannot execute anything."""
    verdict = codeshield.scan("def f(:\n    pass\n")

    assert verdict.allowed
    assert verdict.findings == []


def test_rejection_reads_as_a_repair_instruction():
    verdict = codeshield.scan("import socket\n")

    text = verdict.for_prompt()
    assert "never ran" in text
    assert "socket" in text
    assert "line 1" in text


def test_findings_carry_the_line():
    verdict = codeshield.scan("x = 1\ny = 2\nimport socket\n")

    assert [f.line for f in verdict.findings] == [3]
