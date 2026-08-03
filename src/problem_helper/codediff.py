"""Unified diff between the student's code and the model's code."""

from __future__ import annotations

import difflib


def unified(student_code: str, fixed_code: str, context: int = 3) -> str:
    diff = difflib.unified_diff(
        student_code.splitlines(keepends=True),
        fixed_code.splitlines(keepends=True),
        fromfile="student.py",
        tofile="fixed.py",
        n=context,
    )
    text = "".join(diff)
    if text and not text.endswith("\n"):
        text += "\n"
    return text
