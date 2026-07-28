"""Markdown — the operating rules, and the context assembled around them each run.

The rules used to be a Python constant inside the repair loop. Two reasons they are a file now:
an admin can open it and fix a rule in fifteen seconds without touching code, and it is a layer
that gets re-injected on every run — a rule that lives only in the transcript can disappear when
older turns are summarised, and nothing in the request would say so.

`build_context` is the *push* half of the context: what every run carries whether or not the
request needs it. The *pull* half is the retrieve tools the model calls itself, and it is not
here on purpose — pushing everything is how a context window fills with things nobody read.

Self-test: `python -m memory.rules`
"""

from . import sql
from .db import now, tutor_rules_path
from .events import append_event


def load_rules():
    """Read fresh every call. The staleness of a cached copy is exactly the bug this avoids."""
    path = tutor_rules_path()
    return path.read_text() if path.exists() else ""


def append_rule(line, rationale, run_id=None, proposed_by="curator"):
    """Add one line under `## Learned rules`.

    Only ever called behind a human confirmation — a fact changes one future answer, a rule
    changes every future answer for every student until somebody edits the file back.
    """
    path = tutor_rules_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    text = load_rules() or "# Tutor operating rules\n"
    entry = f"- {line.strip()}  <!-- {proposed_by}, run {run_id}: {rationale.strip()} -->\n"
    if "## Learned rules" not in text:
        text = text.rstrip() + "\n\n## Learned rules\n"
    path.write_text(text.rstrip() + "\n" + entry)
    append_event("rule_added", run_id, {"line": line.strip(), "rationale": rationale.strip(),
                                        "by": proposed_by})
    return {"ok": True}


def build_context(session, problem_id, platform):
    """The pushed block, assembled from named sources.

    The order is the point. The rules are byte-identical for every student and every problem,
    so they go first and stay inside the prefix a provider can cache; everything per-student
    goes last, where it is also at the strong end of the attention curve. Put the student's
    profile on top and the shared prefix differs for every user — the rules are then re-billed
    in full on every request, and the invoice gives no clue why.
    """
    parts = [load_rules().strip() or "(no operating rules file)"]

    mistakes = sql.top_mistakes(session.user_id)
    if mistakes:
        tally = ", ".join(f"{m['mistake_tag']} x{m['n']}" for m in mistakes)
        parts.append("## This student\n\n"
                     f"Recurring defects, most frequent first: {tally}.\n"
                     "Check for these before anything else — it is usually one of them again.")

    spent = sql.hints_spent(session.user_id, problem_id)
    if spent:
        seen = ", ".join(sql.concepts_already_hinted(session.user_id, problem_id))
        parts.append(f"## Already spent on {problem_id}\n\n"
                     f"{spent} hint(s), covering: {seen}.\n"
                     "Do not repeat a concept they have already paid for.")
    return "\n\n".join(parts)


# ---------------------------------------------------------------- tests

if __name__ == "__main__":
    import shutil
    import tempfile
    from pathlib import Path

    from . import db
    from .docs import Session

    tmp = Path(tempfile.mkdtemp(prefix="rules-"))
    db.STATE, db.RULES_DIR = tmp / "state", tmp / "rules"
    db.RULES_DIR.mkdir(parents=True)
    tutor_rules_path().write_text("# Tutor operating rules\n\n- Never show the repaired code.\n")

    passed = failed = 0

    def check(label, condition, detail=""):
        global passed, failed
        passed, failed = passed + bool(condition), failed + (not condition)
        print(("  PASS  " if condition else "  FAIL  ") + label, "" if condition else detail)

    A = Session("u42")

    print("1. an admin edit lands on the next run")
    check("the rules are read from the file", "Never show the repaired code" in load_rules())
    tutor_rules_path().write_text(load_rules() + "- Answer in the student's language.\n")
    check("a hand edit is in the very next context, with no restart",
          "student's language" in build_context(A, "1729A", "codeforces"))

    print("\n2. assembly order")
    sql.record_repair("r1", "u42", "1729A", "codeforces", "off_by_one", "stops one early")
    sql.record_repair("r2", "u42", "1730B", "codeforces", "off_by_one", "again")
    sql.record_hint("r1", "u42", "1729A", "loop_bounds", 20, "which window?")
    ctx = build_context(A, "1729A", "codeforces")

    check("the rules are in it", "Never show the repaired code" in ctx)
    check("the student's tally is in it", "off_by_one x2" in ctx, ctx)
    check("the spent hint and its concept are in it", "1 hint(s)" in ctx and "loop_bounds" in ctx)
    check("the rules come before anything per-student — this is what protects the cache prefix",
          ctx.index("Never show the repaired code") < ctx.index("Recurring defects"))
    check("...and the per-problem block comes last",
          ctx.index("Recurring defects") < ctx.index("Already spent"))

    print("\n3. the empty cases, where string assembly usually leaves debris")
    fresh = build_context(Session("u99"), "1729A", "codeforces")
    check("a student with no history gets the rules and nothing else",
          fresh.strip() == load_rules().strip(), fresh)
    check("no dangling header for a tally that does not exist", "Recurring defects" not in fresh)
    check("no 'None' anywhere", "None" not in fresh and "None" not in ctx)
    check("an unhinted problem has no budget block",
          "Already spent" not in build_context(A, "1730B", "codeforces"))

    print("\n4. appending a rule")
    append_rule("Never explain syntax unless asked.", "two runs where the student asked for ideas",
                run_id="r1")
    check("the line is in the file", "Never explain syntax" in load_rules())
    check("...under the learned heading", load_rules().index("## Learned rules")
          < load_rules().index("Never explain syntax"))
    check("...carrying the run that proposed it and why", "run r1" in load_rules()
          and "asked for ideas" in load_rules())
    check("...and it is attached to the next run", "Never explain syntax"
          in build_context(A, "1729A", "codeforces"))

    missing = tmp / "rules" / "tutor_rules.md"
    missing.unlink()
    check("a missing rules file is visible, not silently empty",
          "(no operating rules file)" in build_context(A, "1729A", "codeforces"))

    shutil.rmtree(tmp)
    print(f"\n{passed} passed, {failed} failed")
