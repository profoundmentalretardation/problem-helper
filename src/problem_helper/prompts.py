"""Prompts for the three agents: fixer, hint generator, hint validator.

The prompts themselves are in English; every agent is told to write its student-facing
text in the language of the problem statement, so a Russian task yields a Russian hint.
"""

from __future__ import annotations

from .sandbox import TestReport
from .schemas import Mistake, TestCase

# --------------------------------------------------------------------------- #
# Agent 1: mistake analysis and repair
# --------------------------------------------------------------------------- #

FIXER_SYSTEM = """\
You are an experienced programming teacher and Python code reviewer.
You are given a problem statement, a student's solution and the results of running it
against tests (stdin → stdout).

Your job:
1. Find the REAL reasons the tests fail, not cosmetic nitpicks.
2. Fix the code with a minimal edit: keep the student's approach, structure and style
   whenever their idea can work at all. Rewrite from scratch only if the approach is
   beyond repair.
3. Return the whole program as a single file: it reads from stdin and prints to stdout.

Answer requirements:
- fixed_code — source only, no markdown wrapper and no explanations inside it.
- mistakes — the student's real mistakes: what is wrong, why it breaks the solution,
  and which line of the student's original code it sits on (0 if not line-specific).
- Write the human-readable fields in the language of the problem statement.\
"""


def _format_tests(tests: list[TestCase], limit: int = 5) -> str:
    shown = tests[:limit]
    blocks = [
        f"Test #{i + 1}:\nstdin:\n{t.input or '<empty>'}\nexpected stdout:\n{t.expected_output}"
        for i, t in enumerate(shown)
    ]
    if len(tests) > limit:
        blocks.append(f"...and {len(tests) - limit} more tests in the same format.")
    return "\n\n".join(blocks)


def fixer_user(
    *,
    task: str,
    student_code: str,
    tests: list[TestCase],
    baseline: TestReport,
    previous_code: str | None = None,
    previous_report: TestReport | None = None,
) -> str:
    parts = [
        f"# Problem statement\n{task}",
        f"# Student's code\n```python\n{student_code}\n```",
        f"# Tests\n{_format_tests(tests)}",
        f"# Test run of the student's code\n{baseline.for_prompt()}",
    ]
    if previous_code is not None and previous_report is not None:
        parts.append(
            "# Your previous fix did not pass the tests\n"
            f"```python\n{previous_code}\n```\n"
            f"Its test run:\n{previous_report.for_prompt()}\n"
            "Work out why that edit failed and propose a different fix."
        )
    return "\n\n".join(parts)


# --------------------------------------------------------------------------- #
# Agent 2: hint generation
# --------------------------------------------------------------------------- #

HINT_SYSTEM = """\
You write a short hint for a student who is solving a programming problem and is stuck
on a mistake. You have the correct solution, but the student will never see it — they
must get there themselves.

Hint rules:
- 1–4 sentences, no filler, no generic advice like "check your logic".
- Point at the specific place and nature of the mistake (you may name the line, the
  variable, the condition, the edge case) and explain WHY it is wrong.
- Do NOT give ready code, do NOT dictate the fixed line verbatim and do NOT restate the
  full solution — the student must make the edit themselves.
- Friendly tone, address the student informally.
- Write the hint in the language of the problem statement.\
"""


def _format_mistakes(mistakes: list[Mistake]) -> str:
    if not mistakes:
        return "<none listed>"
    return "\n".join(f"- [line {m.line or '?'}] {m.title}: {m.detail}" for m in mistakes)


def hint_user(
    *,
    task: str,
    student_code: str,
    fixed_code: str,
    diff: str,
    mistakes: list[Mistake],
    rejected: list[tuple[str, list[str]]] | None = None,
) -> str:
    parts = [
        f"# Problem statement\n{task}",
        f"# Student's code\n```python\n{student_code}\n```",
        f"# Mistakes found by the reviewer\n{_format_mistakes(mistakes)}",
        f"# Reference solution (never show it to the student)\n```python\n{fixed_code}\n```",
        (
            "# Difference between the student's code and the reference\n"
            f"```diff\n{diff or '<empty>'}\n```"
        ),
    ]
    if rejected:
        history = "\n\n".join(
            f"Rejected hint #{i + 1}:\n{hint}\nReviewer remarks:\n"
            + "\n".join(f"- {issue}" for issue in issues)
            for i, (hint, issues) in enumerate(rejected)
        )
        parts.append(
            "# Previous versions of the hint were rejected\n"
            f"{history}\n\n"
            "Take every remark into account and write a new hint."
        )
    return "\n\n".join(parts)


# --------------------------------------------------------------------------- #
# Agent 3: hint validation
# --------------------------------------------------------------------------- #

VALIDATOR_SYSTEM = """\
You are a strict reviewer of hints written for students. You are given the problem
statement, the student's code, the reference solution, the difference between them and
the hint that is about to be shown to the student.

Judge the hint by these criteria:
1. Accuracy — it is about the real mistake visible in the diff, not an invented or
   secondary one.
2. Explicitness — it is concrete and clear: the student will know where to look and what
   is wrong. Vague wording ("think about your algorithm", "check your code") is a failure.
3. No spoiler — it contains no ready code and does not restate the fix verbatim; there is
   still work left for the student.
4. The hint is written in the language of the problem statement and the tone is respectful.

If any criterion is violated, set approved=false and list concrete remarks in issues,
phrased as instructions ("drop the line of code", "state that the loop skips the last
element"). If everything is fine, set approved=true and leave issues empty.\
"""


def validator_user(
    *,
    task: str,
    student_code: str,
    fixed_code: str,
    diff: str,
    hint: str,
) -> str:
    return "\n\n".join(
        [
            f"# Problem statement\n{task}",
            f"# Student's code\n```python\n{student_code}\n```",
            f"# Reference solution\n```python\n{fixed_code}\n```",
            f"# Difference\n```diff\n{diff or '<empty>'}\n```",
            f"# Hint under review\n{hint}",
        ]
    )
