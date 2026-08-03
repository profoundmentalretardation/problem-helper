from problem_helper import codediff


def test_unified_diff_marks_changed_line():
    diff = codediff.unified("a = 1\nprint(a - 1)\n", "a = 1\nprint(a + 1)\n")

    assert "--- student.py" in diff
    assert "+++ fixed.py" in diff
    assert "-print(a - 1)" in diff
    assert "+print(a + 1)" in diff


def test_identical_code_gives_empty_diff():
    assert codediff.unified("x = 1\n", "x = 1\n") == ""
