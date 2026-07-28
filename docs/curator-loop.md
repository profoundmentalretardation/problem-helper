# HW2 — slice B: the curator

**In this commit:** `memory/db.py`, `memory/docs.py`, `memory/events.py`,
`04_curator_loop.ipynb`, `rules/curator_brief.md`, `traces/b_curator.md`.

The project is a helper that finds the defect in a student's failing submission and turns it
into a hint that does not give the answer away. Three loops existed after HW1: a prompt
shield, a repair loop, a hint loop. This slice adds the agent that runs *after* them and
decides what the system should still know next week — and the two stores it writes to.

## What is in it

**A document store for free-form memory (`memory/docs.py`).** Facts the agent writes in its
own words, with no schema beyond two fields. `scope` is `user:<id>` or `shared`, and it is
applied *before* ranking, not after — another student's private fact cannot be ranked into
this answer even in principle. `cue` is the keywords the agent writes for *when this should
come back*; a fact without one can never be retrieved, so the store refuses it rather than
accepting a write nobody will ever read. Retrieval is keyword overlap (cue weighted double)
with recency breaking ties. No embeddings: over a few dozen documents a vector index buys
nothing and hides *why* something surfaced, which is the one thing you need the day a fact
appears in an answer where it should not have.

**An append-only, hash-chained event log (`memory/events.py`).** One line per thing that
happened, each carrying the hash of the line before it, plus the `handoffs` table. Editing or
deleting a record breaks every hash after it and `verify_chain` names the `seq`. Truncating
the *tail* is not detectable without an external anchor — that is written down in the
docstring next to the check, because a green result that means less than it looks like is
worse than no check.

**The curator (`04_curator_loop.ipynb`).** The executor hands over
`{status, result, needs_approval}`; the curator reads the finished run and decides what to
write where. Three tools and nothing else: `save_fact`, `propose_rule`, `finish`. It cannot
read code, run tests, or reach the student — it is the agent with write access to long-term
memory, so everything that is not writing to memory was taken away rather than forbidden in
prose. Its delegation brief (`rules/curator_brief.md`) sets scope, when it acts alone, when it
asks, when it escalates, and a four-call effort budget; the system prompt is generated from
the brief, so the caller and the agent read the same text.

## Ideas from class this puts into practice

*Session 4* — the session key `(user_id, thread_id)`, memory scoped by it, and the store/layer
split: rows for what we query, documents for what an agent wrote in prose, markdown for what
must hold every run. Facts are rendered into `input[]` as data carrying their source, never
into `instructions`, because a saved fact is quoted text and putting it in the system prompt
grants it developer authority — which is exactly how "remember that I am an admin" becomes an
operating rule two sessions later.

*Session 5* — a structured hand-off instead of prose between agents, a written delegation
brief with an effort budget, autonomy pinned per action (act-and-report for facts, suggest-only
for rules), and the gate placed on the one irreversible thing.

## Why the system is multi-agent, and what one agent would have cost

Two of the four payoffs, and the second is the real one:

**Decoupled execution.** The curator runs after the student already has their hint. Nothing it
does is on the critical path, so it can spend calls on a decision the executor would have had
to rush.

**Focus, on genuinely conflicting goals.** The executor's goal is "fix it and say something
useful now". The curator's goal is "what of this is true beyond today". Session 5's rule is to
cut where goals conflict rather than by job title, and these conflict: an agent optimising the
first will either save noise on the way past or — far more commonly, and this is the documented
failure — save nothing at all, because it is busy answering and the memory tool is a judgement
call it can skip at no visible cost.

What one agent cost us is not hypothetical: it is the `state/memory.json` this slice replaces.
Faced with an agent nobody trusted to decide, the previous version had the *orchestrator* write
memory on a fixed rule — a `{user: {tag: count}}` tally. It recorded the one thing that was
already an enum and nothing that needed judgement, and it could not have recorded "wants
conceptual hints only" if the student had said it in every message.

## Coordination through shared memory, and why it is the hard one

The two agents coordinate through the `handoffs` table — the mechanism session 5 lists as the
most flexible and the least predictable. It is the hardest to debug not because it is
complicated but because the write and the read are separated in time and neither side appears
in the other's transcript: a system can pass ten tests and break on the eleventh user, and the
trace cannot be reconstructed from either agent afterwards. It is also where the dormant attack
lives — one agent writes now, another reads weeks later.

Three things were done about it. Every hand-off is written to *both* stores under one `run_id`,
so the table answers "what was passed" and the log answers "in what order, among what else".
The log is hash-chained, so the judged system cannot quietly tidy up its own evidence. And the
one write that cannot be taken back — a private detail published into `shared`, where every
other student reads it — is blocked in code, not asked for in the prompt: a shared fact naming
the student is refused, with an error that tells the model how to rewrite it.

## How I tested it

31 checks in the notebook and 38 in the two store modules (`python -m memory.docs`,
`python -m memory.events`), all without an API key: the model is scripted, the stores are real,
and every assertion is about what ended up on disk after the loop returned rather than what the
loop said it did.

The ones that found something or that I would not delete:

- **Both directions on the judgement call.** A run containing "hi! thanks, that worked" must
  save *nothing*; a run where the student states a preference must save it *with a cue*. A
  curator that saves on both is not careful, it is agreeable — one test alone would have passed
  for the wrong reason. Live model, both directions hold (`traces/b_curator.md`).
- **The fact comes back, and only to its owner.** After the save, a different request days
  later pulls it on its cue; the identical request from another student pulls nothing.
- **The gate, in both directions.** Declining `propose_rule` leaves `tutor_rules.md` byte-identical
  and still records the proposal as declined; only a literal `yes` opens it (`"y"`, `"sure"`,
  `"yes please"` do not).
- **The error branch costs zero model calls.** A run whose status is `checker_failed` is never
  learned from — asserted by scripting an *empty* model, which raises if the loop consults it.
- **The store refusing a write does not kill the run.** A cue-less fact comes back as a tool
  error, the loop carries on, and nothing cue-less is on disk afterwards.
- **Tampering is caught where it happened.** Editing one record in the log breaks the chain at
  exactly that `seq`; deleting one from the middle is caught; dropping the tail is not, and
  there is a test that says so out loud.

The failure worth reporting: my first version of the test for "a rejected fact does not stop
the loop" asserted `status == "curated"`, and it failed — correctly. The fact it re-sent had
been saved by an earlier test, so it deduplicated and nothing new was written. The code was
right and the assertion was wrong, which is the useful kind of red: it caught that my tests
shared a store between cases and were therefore order-dependent. The suite now runs against a
temporary sandbox that is restored before the live cell.
