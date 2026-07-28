"""SQLite — the domain model.

What the project is *about*: submissions students sent, defects we verified and repaired, and
hints we spent. Rows with columns, because every one of these is something we filter, group or
count by. The store this replaces was `state/memory.json`, holding

    {"u42": {"off_by_one": 3}}

which answered exactly one question and threw away everything needed to answer any other. The
count is still one `GROUP BY` away; what the rows add is *which problems*, *when*, and *what
the defect actually was* — which is what the next session's context needs.

Nothing here is written by a model. `record_repair` is called by the orchestrator after the
tests passed: "this defect was real" is a fact about a test run, not a judgement, and the loops
already refuse to let the model decide whether its own patch worked.

Self-test: `python -m memory.sql`
"""

from .db import connect, now

MISTAKE_TAGS = ["off_by_one", "uninitialized", "overflow", "wrong_type",
                "missing_edge_case", "io_format", "wrong_algorithm"]

SCHEMA = """
CREATE TABLE IF NOT EXISTS problems (
    problem_id TEXT NOT NULL,
    platform   TEXT NOT NULL,
    title      TEXT,
    topic      TEXT,
    PRIMARY KEY (problem_id, platform)
);

CREATE TABLE IF NOT EXISTS submissions (
    submission_id TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    problem_id    TEXT NOT NULL,
    platform      TEXT NOT NULL,
    at            TEXT NOT NULL,
    passed        INTEGER NOT NULL,
    total         INTEGER NOT NULL,
    code          TEXT NOT NULL
);

-- One row per verified defect. Primary key is (run_id, mistake_tag) so a rerun of the same
-- run cannot inflate the tally, while two genuinely different defects in one run still fit.
CREATE TABLE IF NOT EXISTS repairs (
    run_id      TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    problem_id  TEXT NOT NULL,
    platform    TEXT NOT NULL,
    mistake_tag TEXT NOT NULL,
    diagnosis   TEXT NOT NULL,
    status      TEXT NOT NULL,
    at          TEXT NOT NULL,
    PRIMARY KEY (run_id, mistake_tag)
);

CREATE TABLE IF NOT EXISTS hints (
    hint_id    INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    concept    TEXT NOT NULL,
    directness INTEGER,
    hint_text  TEXT NOT NULL,
    at         TEXT NOT NULL
);
"""


def _con():
    con = connect()
    con.executescript(SCHEMA)
    return con


# ---------------------------------------------------------------- writes

def record_submission(submission_id, user_id, problem_id, platform, at, passed, total, code):
    with _con() as con:
        con.execute("INSERT OR REPLACE INTO submissions VALUES (?,?,?,?,?,?,?,?)",
                    (submission_id, user_id, problem_id, platform, at, passed, total, code))
    return {"ok": True}


def record_repair(run_id, user_id, problem_id, platform, mistake_tag, diagnosis, status="fixed"):
    """A defect that was found, patched, and confirmed by running the tests."""
    if mistake_tag not in MISTAKE_TAGS:
        # A store that quietly accepts anything makes every later query a lie: the tally would
        # split across "off_by_one" and "offbyone" and neither number would mean anything.
        return {"ok": False, "error": f"unknown mistake_tag {mistake_tag!r}, expected one of "
                                      f"{MISTAKE_TAGS}"}
    with _con() as con:
        con.execute("INSERT OR REPLACE INTO repairs VALUES (?,?,?,?,?,?,?,?)",
                    (run_id, user_id, problem_id, platform, mistake_tag, diagnosis, status, now()))
    return {"ok": True}


def record_hint(run_id, user_id, problem_id, concept, directness, hint_text):
    """Called by whoever delivered a hint.

    Loop 2 still appends to `state/hints.jsonl` and is deliberately left alone — its 22 tests
    are about the writer/checker pair, and wiring a database into them would make every test
    run write to a real store. The caller that owns the pipeline records the hint here; the
    seam is visible on purpose rather than hidden inside the loop.
    """
    with _con() as con:
        con.execute("INSERT INTO hints (run_id, user_id, problem_id, concept, directness, "
                    "hint_text, at) VALUES (?,?,?,?,?,?,?)",
                    (run_id, user_id, problem_id, concept, directness, hint_text, now()))
    return {"ok": True}


# ---------------------------------------------------------------- reads

def top_mistakes(user_id, n=3):
    """The student's recurring defects, most frequent first.

    This is the whole tally the old JSON file held, as one query — and the same table still
    answers everything that file could not.
    """
    with _con() as con:
        rows = con.execute(
            "SELECT mistake_tag, COUNT(*) AS n, MAX(at) AS last_seen FROM repairs "
            "WHERE user_id = ? GROUP BY mistake_tag ORDER BY n DESC, last_seen DESC LIMIT ?",
            (user_id, n)).fetchall()
    return [dict(r) for r in rows]


def problems_with_mistake(user_id, mistake_tag):
    with _con() as con:
        rows = con.execute("SELECT DISTINCT problem_id FROM repairs WHERE user_id = ? AND "
                           "mistake_tag = ? ORDER BY problem_id", (user_id, mistake_tag)).fetchall()
    return [r["problem_id"] for r in rows]


def hints_spent(user_id, problem_id):
    with _con() as con:
        return con.execute("SELECT COUNT(*) AS n FROM hints WHERE user_id = ? AND problem_id = ?",
                           (user_id, problem_id)).fetchone()["n"]


def concepts_already_hinted(user_id, problem_id):
    with _con() as con:
        rows = con.execute("SELECT DISTINCT concept FROM hints WHERE user_id = ? AND "
                           "problem_id = ? ORDER BY concept", (user_id, problem_id)).fetchall()
    return [r["concept"] for r in rows]


def best_submission(user_id, problem_id, platform):
    """Most tests passed; the most recent attempt breaks a tie."""
    with _con() as con:
        row = con.execute("SELECT * FROM submissions WHERE user_id = ? AND problem_id = ? AND "
                          "platform = ? ORDER BY passed DESC, at DESC LIMIT 1",
                          (user_id, problem_id, platform)).fetchone()
    return dict(row) if row else None


# ---------------------------------------------------------------- tests

if __name__ == "__main__":
    import shutil
    import tempfile
    from pathlib import Path

    from . import db

    db.STATE = tmp = Path(tempfile.mkdtemp(prefix="sql-"))
    passed = failed = 0

    def check(label, condition, detail=""):
        global passed, failed
        passed, failed = passed + bool(condition), failed + (not condition)
        print(("  PASS  " if condition else "  FAIL  ") + label, "" if condition else detail)

    print("1. repairs are rows, and the rows outlive the question they were written for")
    record_repair("r1", "u42", "1729A", "codeforces", "off_by_one", "loop stops one early")
    record_repair("r2", "u42", "1730B", "codeforces", "off_by_one", "same, on a prefix sum")
    record_repair("r3", "u42", "1731C", "codeforces", "io_format", "read the ints as strings")
    record_repair("r4", "u77", "1729A", "codeforces", "overflow", "int32 overflowed")

    top = top_mistakes("u42")
    check("the tally the old JSON file held is one query",
          (top[0]["mistake_tag"], top[0]["n"]) == ("off_by_one", 2), top)
    check("...and it is ordered by frequency", [t["mistake_tag"] for t in top]
          == ["off_by_one", "io_format"], top)
    check("the same rows also say on which problems — the JSON file could not",
          problems_with_mistake("u42", "off_by_one") == ["1729A", "1730B"])
    check("...and when it last happened", top[0]["last_seen"].endswith("Z"))
    check("one student's tally never contains another's mistakes",
          all(t["mistake_tag"] != "overflow" for t in top), top)
    check("a student with no history gets an empty list, not an error", top_mistakes("u99") == [])

    print("\n2. the enum is enforced at the store, not in a prompt")
    bad = record_repair("r5", "u42", "1732D", "codeforces", "offbyone", "typo'd tag")
    check("a tag outside the enum is refused", bad["ok"] is False, bad)
    check("...the error says what was expected", "off_by_one" in bad["error"])
    check("...and nothing was written", top_mistakes("u42")[0]["n"] == 2)

    print("\n3. a rerun does not inflate the tally")
    record_repair("r1", "u42", "1729A", "codeforces", "off_by_one", "loop stops one early")
    check("the same run recorded twice still counts once", top_mistakes("u42")[0]["n"] == 2)
    record_repair("r1", "u42", "1729A", "codeforces", "io_format", "a second, different defect")
    check("...but two different defects in one run both count",
          sum(t["n"] for t in top_mistakes("u42")) == 4)

    print("\n4. hints are a budget, per problem")
    record_hint("r1", "u42", "1729A", "loop_bounds", 20, "which window is never scored?")
    check("a spent hint is counted", hints_spent("u42", "1729A") == 1)
    check("a different problem has its own budget", hints_spent("u42", "1730B") == 0)
    check("a different student has their own budget", hints_spent("u77", "1729A") == 0)
    check("the concept is remembered, so the next hint does not repeat it",
          concepts_already_hinted("u42", "1729A") == ["loop_bounds"])
    record_hint("r9", "u42", "1729A", "edge_case", 30, "what if k == n?")
    check("two hints, two concepts",
          (hints_spent("u42", "1729A"), concepts_already_hinted("u42", "1729A"))
          == (2, ["edge_case", "loop_bounds"]))

    print("\n5. best submission: most tests passed, most recent on a tie")
    record_submission("s100", "u42", "1729A", "codeforces", "2026-07-20T10:00Z", 1, 5, "print(0)")
    record_submission("s101", "u42", "1729A", "codeforces", "2026-07-20T11:30Z", 3, 5, "buggy")
    record_submission("s102", "u42", "1729A", "codeforces", "2026-07-20T12:05Z", 3, 5, "buggy2")
    check("the most recent of the two best is chosen",
          best_submission("u42", "1729A", "codeforces")["submission_id"] == "s102")
    check("no submissions is None, not a crash",
          best_submission("u99", "1729A", "codeforces") is None)

    shutil.rmtree(tmp)
    print(f"\n{passed} passed, {failed} failed")
