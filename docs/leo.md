# HW2 — slice A: the domain model and context assembly

**In this commit:** `memory/sql.py`, `memory/rules.py`, `rules/tutor_rules.md`,
`01_repair_loop.ipynb`, `traces/a_push_pull.md`.

The project helps a student find the defect in their own failing submission without showing
them the answer. After HW1 it had three loops and one piece of memory: a file called
`state/memory.json` holding `{"u42": {"off_by_one": 3}}`. This slice replaces that with stores
that fit what they hold, and rebuilds the context the repair loop runs on.

## What is in it

**A relational store for the domain model (`memory/sql.py`).** Three tables: `submissions`,
`repairs`, `hints`. The one that matters is `repairs` — one row per verified defect, with the
problem, the platform, the diagnosis and the run it came from. The old tally answered "how
often"; the same data as rows also answers "on which problems, when, and what was it", and
that is what the next session's context needs. `top_mistakes()` is a `GROUP BY ... ORDER BY
COUNT(*)`, so the tally is still one query away — it was never the counting that was wrong, it
was throwing everything else away to get it.

`record_repair()` rejects a `mistake_tag` outside the enum instead of storing it, and it is
called by the orchestrator after the tests passed, never by the model. "This defect was real"
is a fact about a test run, not a judgement, and the loop already had that rule for
verification — this keeps it for memory.

**Operating rules in markdown (`memory/rules.py`, `rules/tutor_rules.md`).** The tutor's rules
used to be a Python constant inside the notebook. They are now a file an admin opens and fixes,
read fresh on every run, so an edit lands on the next run with no restart and no deploy.

**Context assembled from named sources (`01_repair_loop.ipynb`).** `build_context()` composes
the pushed half every run: the rules, then this student's recurring mistakes, then what they
have already spent on this problem. The order is deliberate — the rules are byte-identical for
every student, so they go first and stay inside the cacheable prefix, and everything per-student
goes last, where it also sits at the end of the attention curve. Put the profile on top and
every request pays full price for the rules again.

The pulled half is two tools the model calls itself: `retrieve_memory(query)` and
`read_problem_notes(problem_id)`. Push is what must be there whether or not the request needs
it; pull is what is too heavy to carry and too rare to justify.

## Ideas from class this puts into practice

Session 4, almost end to end. The four state layers, and one store per layer rather than one
store for everything — a table for what we query, markdown for what must hold every run.
"Memory is not a second copy of your database": the mistake tag is a row, so nothing writes it
into the memory store as well. Push versus pull as a decision with a cost on each side, rather
than a habit. The prefix-cache ordering rule, which is why the assembly order above is what it
is. And the reason rules live in a re-injected layer at all: a rule that exists only in the
transcript can vanish when older turns are summarised, and nothing in the request would say so.

## Why our system is shaped this way

The system is an executor (shield → repair → hint), a curator that runs after it, and a monitor
on its own clock. Two of the four payoffs from session 5:

**Focus, on conflicting goals.** The executor's goal is "fix it and say something useful now".
The curator's goal is "what of this is true beyond today". Session 5's rule is to cut where
goals conflict rather than by job title, and these genuinely conflict.

**Decoupled execution.** The curator runs after the student already has their hint; the monitor
runs over logs after the fact. Neither is on the critical path of a live answer.

What one agent cost is visible in the file this slice deletes. Nobody trusted the busy agent to
decide what to remember, so the orchestrator wrote memory on a fixed rule — a tag counter. It
recorded the one thing that was already an enum and nothing that took judgement.

## How I tested it

60 checks: 26 in `01_repair_loop.ipynb`, 18 in `python -m memory.sql`, 16 in
`python -m memory.rules`. All run without an API key — the model is scripted, the stores are
real, pointed at a temporary directory, and each assertion is about what is on disk after the
call rather than what the function returned.

What I made sure of, and why each one is there:

- **The tally does not mix two students.** `top_mistakes("u42")` never returns a tag only u77
  ever made. This is the leak from session 4 in its durable form, so it is asserted directly
  rather than assumed from the `WHERE` clause being present.
- **The rows answer what the counter could not.** After three repairs, a query returns the two
  problems `off_by_one` happened on. That is the whole justification for the rewrite, so it is
  a test and not a sentence in a README.
- **An invalid tag is refused, not stored.** A tag outside the enum comes back as an error;
  nothing is written. A store that silently accepts anything makes every later query a lie.
- **An admin edit lands on the next run.** Write a new line into `rules/tutor_rules.md`, call
  `build_context()` again, and the line is in it — no restart, no reimport.
- **Assembly order is asserted on indices, not by eye.** The index of the rules block is less
  than the index of the per-student block. Reading the printed context and nodding is not a
  test; the ordering is the thing that protects the cache prefix, so it gets a real assertion.
- **A student with no history gets the rules and nothing else.** The empty case is where
  string-assembly code usually leaves a dangling header or a stray "None".

One test failed while writing this, and it is worth the space because of what it turned out to
be. The test for the pull tools scripted the model three turns — retrieve memory, read notes,
propose the fix — and the run died on `the loop asked for more replies than the script has`.
The loop was right: a verified fix does not end it, only a submission or a stop condition does,
so it went round again and found an empty script. The test had encoded an assumption about the
control flow that the control flow never made. That is the standing risk of scripted-model
tests, and it is why the fix was to cap `max_steps` in that one test rather than to "fix" the
loop into stopping early.

The rerun case is the one I would point at second, and it is a design decision the tests pin
rather than a bug they caught: `record_repair` is keyed on `(run_id, mistake_tag)` with `INSERT
OR REPLACE`, so re-executing a notebook cell cannot inflate a student's tally, while two
genuinely different defects in one run still both count. Both directions are asserted, because
only the first would have been satisfied by a store that silently dropped the second defect.

**Not done yet:** the hint is recorded into SQLite by the caller rather than by loop 2 itself,
because wiring a store into loop 2 would make its 22 existing tests write to a real database on
every run. The seam is visible in `sql.record_hint`'s docstring rather than hidden. The three
hint budget is also not enforced anywhere — it is a line in `tutor_rules.md` that the model can
read and ignore, and `hints_spent()` exists to enforce it in code, which is the obvious next
step.
