# %% [markdown]
# # Loop 3 — the curator: what, out of a finished run, is worth remembering
#
# Loops 1 and 2 fixed a defect and sent one hint. This runs afterwards, on the finished run,
# and decides what the system should still know about this student next week.
#
# It is a second agent, and it earns its place on two of the four payoffs from session 5:
#
# * **decoupled execution** — it runs after the student already has their hint. Nothing it
#   does is on the critical path, so it can afford to think about a decision the executor
#   would have had to rush.
# * **focus** — the executor's goal is "fix it and say something useful". The curator's goal
#   is "what of this is true beyond today". Those goals conflict: an agent optimising the
#   first will either save noise on the way past or, far more often, save nothing at all
#   because it is busy. A conflict of goals is the right place to cut.
#
# What one agent would have cost: exactly the failure the current `state/memory.json` shows —
# the tally is written by the orchestrator on a fixed rule, because nobody trusted the busy
# agent to decide, and so it records one enum and nothing that needed judgement.
#
# Assignment checklist:
#
# * **the pair** — executor -> curator, passing `{status, result, needs_approval}` and
#   branching on the fields, never on each other's prose
# * **the brief** — `rules/curator_brief.md`: scope, acts alone, asks, escalates, and a
#   four-call effort budget
# * **shared memory** — they coordinate through the `handoffs` table and the event log, both
#   keyed by `run_id`
# * **the model decides when to save** — noise is dropped, a real preference is kept, and the
#   cue it writes is what brings the fact back
# * **the gate** — `propose_rule` asks a human; a rule is every future answer, a fact is one
# * **error branch** — a run that ended in an error is never learned from

# %%
import json
import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from openai import OpenAI

# Works from the repo root (Jupyter) and from scratch/ (`python nb4.py`).
ROOT = Path.cwd() if (Path.cwd() / "memory").is_dir() else Path.cwd().parent
sys.path.insert(0, str(ROOT))
os.chdir(ROOT)

from memory import events                                    # noqa: E402
from memory.docs import SHARED, Session, render_facts, retrieve_facts, save_fact  # noqa: E402

load_dotenv()

BRIEF = Path("rules/curator_brief.md")
TUTOR_RULES = Path("rules/tutor_rules.md")

OFFLINE = not os.getenv("LLM_API_KEY")
client = OpenAI(base_url=os.getenv("LLM_BASE_URL"), api_key=os.getenv("LLM_API_KEY") or "offline")
MODEL_CURATOR = os.getenv("MODEL_CURATOR", "anthropic/claude-sonnet-5")

# %% [markdown]
# ## The hand-off
#
# What the executor passes over. `status` is an enum we branch on; `result` is the run; the
# curator never receives, and never asks for, the executor's transcript — it gets the
# conclusions, which is the difference between a hand-off and a shared context window.
#
# If loop 1 has run, `state/loop1_result.json` is on disk and we use it. Otherwise the
# fixture, so this notebook runs on its own like the other three.

# %%
FIXTURE_ENVELOPE = {
    "status": "hint_delivered",
    "result": {
        "run_id": "u42-1729A-20260728T120000Z",
        "user_id": "u42",
        "problem_id": "1729A",
        "platform": "codeforces",
        "mistake_tag": "off_by_one",
        "diagnosis": "The sliding window loop stops one iteration early, so the last window "
                     "is never scored.",
        "hint": "Your loop covers every window but one. Which window never gets scored, and "
                "when would that change the answer?",
        "concept": "loop_bounds",
        "rejected": [{"hint": "Your loop finishes one step early near the end.",
                      "by": "model", "feedback": "too close to naming the edit"}],
        "student_said": "i always write the c++ version first and then rewrite it in python "
                        "for the easy ones, and i keep messing up reading the input. also "
                        "please stop telling me about syntax, i want the idea only.",
    },
    "needs_approval": False,
}

NOISE_ENVELOPE = {
    "status": "hint_delivered",
    "result": {**FIXTURE_ENVELOPE["result"],
               "run_id": "u42-1801B-20260728T130000Z",
               "problem_id": "1801B",
               "student_said": "hi! thanks, that worked. see you"},
    "needs_approval": False,
}

BROKEN_ENVELOPE = {"status": "checker_failed",
                   "result": {**FIXTURE_ENVELOPE["result"], "run_id": "u42-1802C-broken"},
                   "needs_approval": False}


def envelope_from_disk():
    """Loop 1's output, wrapped in the hand-off shape. Loop 1 belongs to another slice, so we
    read what it leaves on disk rather than importing it."""
    # Loop 1 writes two files: the flat one loop 2 has parsed since HW1, and this envelope.
    handoff = Path("state/loop1_envelope.json")
    if not handoff.exists():
        return None
    env = json.loads(handoff.read_text())
    return {**env, "result": {**FIXTURE_ENVELOPE["result"], **env["result"]}}


ENVELOPE = envelope_from_disk() or FIXTURE_ENVELOPE
print("input from", "loop 1" if envelope_from_disk() else "fixture")

# %% [markdown]
# ## What the curator sees
#
# The brief, then the run, then what is already remembered. The brief is byte-identical on
# every run and goes first, where the cache can reuse it; the per-run and per-student parts go
# last. Handing it the current memory is what stops it re-saving the same fact in three
# wordings — dedup in the store catches identical text, not a paraphrase.

# %%
def load_history(user_id):
    """The student's recurring mistakes.

    Slice A owns `memory/sql.py`. Until it lands, the caller passes `history` in the envelope
    and this returns None — the notebook prints which one it used rather than pretending.
    """
    try:
        from memory import sql
    except ImportError:
        return None
    return sql.top_mistakes(user_id)


def summarise(envelope, session):
    r = envelope["result"]
    history = r.get("history") or load_history(r["user_id"]) or []
    known = retrieve_facts(session, f"{r['problem_id']} {r['mistake_tag']} "
                                    f"{r.get('student_said', '')}", limit=5)

    lines = [f"## The run\n",
             f"status: {envelope['status']}",
             f"problem: {r['problem_id']} on {r['platform']}",
             f"verified defect: {r['mistake_tag']} — {r['diagnosis']}",
             f"hint delivered: {r.get('hint') or '(none)'}",
             f"concept: {r.get('concept') or '(none)'}"]
    if r.get("rejected"):
        lines.append(f"hints rejected before that one: {len(r['rejected'])}")
    if r.get("student_said"):
        lines.append(f"\nwhat the student said, verbatim:\n\"{r['student_said']}\"")
    if history:
        tally = ", ".join(f"{h['mistake_tag']} x{h['n']}" for h in history)
        lines.append(f"\nthis student's record so far: {tally}")
    lines.append("\n## Already remembered\n")
    lines.append(render_facts(known) or "(nothing on file for this student yet)")
    return "\n".join(lines)


SYSTEM = (BRIEF.read_text() if BRIEF.exists() else "") + """

You are the curator described above. Answer only with tool calls. When you are done — and
doing nothing at all is a normal outcome — call finish with your reason.
"""

# %% [markdown]
# ## Tools
#
# Three, and no more. The curator cannot read code, cannot run tests, cannot message the
# student and cannot reach the executor's tools: it is the agent that writes to long-term
# memory, so the thing to remove from it is every capability that is not writing to memory.
#
# `scope` is an enum rather than a free string because the difference between the two values
# is "one student sees this" and "everyone sees this", and that is not a decision to leave to
# a typo.

# %%
CURATOR_TOOLS = [
    {"type": "function", "function": {
        "name": "save_fact",
        "description": "Write one thing worth remembering into long-term memory. Call it only "
                       "for something that would otherwise be re-asked or re-discovered. Do "
                       "not call it for what a table already holds (the mistake tag, the hint, "
                       "the test results), and do not call it for pleasantries.",
        "parameters": {
            "type": "object",
            "properties": {
                "text": {"type": "string",
                         "description": "The fact, in your own words, one or two sentences."},
                "cue": {"type": "array", "items": {"type": "string"},
                        "description": "2-4 keywords describing WHEN this should come back. "
                                       "Write them for the future request that should pull it."},
                "scope": {"type": "string", "enum": ["private", "shared"],
                          "description": "private: about this student. shared: true about the "
                                         "problem itself and useful to every student."},
                "kind": {"type": "string", "enum": ["fact", "preference", "context"]},
            },
            "required": ["text", "cue", "scope", "kind"],
        },
    }},
    {"type": "function", "function": {
        "name": "propose_rule",
        "description": "Propose a line for the tutor's operating rules. A rule is attached to "
                       "every future run for every student, so this needs a human and you "
                       "cannot apply it yourself. Propose one only when you can name two or "
                       "more runs it would have changed.",
        "parameters": {
            "type": "object",
            "properties": {
                "line": {"type": "string", "description": "The rule, imperative, one line."},
                "rationale": {"type": "string",
                              "description": "Which runs it would have changed, and how."},
            },
            "required": ["line", "rationale"],
        },
    }},
    {"type": "function", "function": {
        "name": "finish",
        "description": "End your turn on this run. Call it when there is nothing left worth "
                       "saving — including immediately, when there was nothing to begin with.",
        "parameters": {
            "type": "object",
            "properties": {"reason": {"type": "string", "description": "One sentence."}},
            "required": ["reason"],
        },
    }},
]

# %% [markdown]
# ## The gate
#
# A fact changes one future answer; a rule changes every future answer, for every student,
# until somebody edits the file. That asymmetry is the whole placement of this gate — and it
# is why `save_fact` runs unattended. A gate on every write is a gate nobody reads.

# %%
def is_yes(answer):
    return answer.strip().lower() == "yes"


def ask_human(question):
    return is_yes(input(f"{question}\nType 'yes' to apply it: "))


def apply_rule(line, rationale, run_id):
    """Append to the markdown rules file.

    Slice A owns `memory/rules.py`; this writes the same file in the same format so the two
    do not have to land in order.
    """
    TUTOR_RULES.parent.mkdir(parents=True, exist_ok=True)
    text = TUTOR_RULES.read_text() if TUTOR_RULES.exists() else "# Tutor operating rules\n"
    entry = f"- {line.strip()}  <!-- curator, run {run_id}: {rationale.strip()} -->\n"
    if "## Learned rules" not in text:
        text = text.rstrip() + "\n\n## Learned rules\n\n"
    TUTOR_RULES.write_text(text.rstrip() + "\n" + entry)
    events.append_event("rule_added", run_id, {"line": line.strip(), "rationale": rationale.strip()})


# %% [markdown]
# ## The loop
#
# | stop | status |
# |---|---|
# | `finish`, having saved something | `curated` |
# | `finish`, having saved nothing | `nothing_to_save` |
# | four tool calls used | `budget_exhausted` |
# | the model answered in prose | `gave_up` |
# | the executor's run ended in an error | `skipped_error_run` |
#
# The last one is a branch in code, before the model is called at all. A run where the test
# runner broke or the checker could not be read says nothing reliable about the student, and a
# fact learned from a broken run is worse than no fact — it is wrong and it is durable.

# %%
LEARNABLE = {"hint_delivered", "approved_not_delivered", "fixed_not_submitted", "submitted"}


def chat(messages, tools, model=MODEL_CURATOR):
    r = client.chat.completions.create(model=model, messages=messages, tools=tools,
                                       tool_choice="auto", temperature=0.2)
    m = r.choices[0].message
    calls = []
    for tc in m.tool_calls or []:
        try:
            args = json.loads(tc.function.arguments)
        except json.JSONDecodeError:
            args = None
        calls.append({"id": tc.id, "name": tc.function.name, "args": args})
    return {"text": m.content, "calls": calls}


def run_curator(envelope, chat_fn=chat, budget=4, confirm=ask_human):
    r = envelope["result"]
    run_id, user_id = r["run_id"], r["user_id"]
    session = Session(user_id)

    events.record_handoff(run_id, "executor", "curator", envelope)

    if envelope["status"] not in LEARNABLE:
        events.append_event("run_finished", run_id,
                            {"agent": "curator", "status": "skipped_error_run"})
        return {"status": "skipped_error_run", "saved": [], "proposed": [], "calls": 0}

    messages = [{"role": "system", "content": SYSTEM},
                {"role": "user", "content": summarise(envelope, session)}]
    saved, proposed, calls = [], [], 0

    while True:
        out = chat_fn(messages, CURATOR_TOOLS)
        if not out["calls"]:
            # Prose is not a decision. We do not read it and guess what it meant.
            events.append_event("run_finished", run_id, {"agent": "curator", "status": "gave_up"})
            return {"status": "gave_up", "saved": saved, "proposed": proposed, "calls": calls}

        messages.append({"role": "assistant", "content": out["text"], "tool_calls": [
            {"id": c["id"], "type": "function",
             "function": {"name": c["name"], "arguments": json.dumps(c["args"])}}
            for c in out["calls"]]})

        for c in out["calls"]:
            def answer(payload):
                messages.append({"role": "tool", "tool_call_id": c["id"],
                                 "content": json.dumps(payload)})

            calls += 1
            if calls > budget:
                events.append_event("run_finished", run_id,
                                    {"agent": "curator", "status": "budget_exhausted"})
                return {"status": "budget_exhausted", "saved": saved, "proposed": proposed,
                        "calls": calls - 1}

            if c["args"] is None:
                answer({"ok": False, "error": "your arguments were not valid JSON, resend"})

            elif c["name"] == "finish":
                events.append_event("run_finished", run_id,
                                    {"agent": "curator", "saved": len(saved),
                                     "reason": c["args"].get("reason", "")})
                return {"status": "curated" if saved else "nothing_to_save",
                        "saved": saved, "proposed": proposed, "calls": calls,
                        "reason": c["args"].get("reason", "")}

            elif c["name"] == "save_fact":
                a = c["args"]
                scope = SHARED if a.get("scope") == "shared" else session.scope
                if scope == SHARED and user_id.lower() in a.get("text", "").lower():
                    # The one write that cannot be taken back: a private detail published to
                    # every other student. Cheap check, and the model gets told why.
                    answer({"ok": False, "error": "that names the student, so it cannot be "
                                                  "shared. Save it as private, or rewrite it "
                                                  "to be about the problem only."})
                    continue
                res = save_fact(scope, a.get("text", ""), a.get("cue"),
                                f"curator:{run_id}", kind=a.get("kind", "fact"))
                if res["ok"] and not res.get("deduplicated"):
                    saved.append({"id": res["fact_id"], "scope": scope, "text": a["text"],
                                  "cue": a["cue"]})
                answer(res)

            elif c["name"] == "propose_rule":
                a = c["args"]
                events.record_handoff(run_id, "curator", "admin",
                                      {"status": "rule_proposed", "result": a,
                                       "needs_approval": True})
                if confirm(f"Add this to the tutor's operating rules? It applies to every "
                           f"student on every future run.\n\n  {a['line']}\n\n"
                           f"  why: {a['rationale']}\n"):
                    apply_rule(a["line"], a["rationale"], run_id)
                    proposed.append({**a, "applied": True})
                    answer({"ok": True, "applied": True})
                else:
                    proposed.append({**a, "applied": False})
                    answer({"ok": True, "applied": False,
                            "note": "a human declined it — do not propose it again this run"})

            else:
                answer({"ok": False, "error": f"no tool called {c['name']}"})


# %% [markdown]
# ## Tests
#
# The model is scripted; the stores are real. Every assertion below is about what ended up on
# disk after the loop returned, not about what the loop said it did.

# %%
import shutil        # noqa: E402
import tempfile      # noqa: E402

from memory import db  # noqa: E402

passed = failed = 0


def check(label, condition, detail=""):
    global passed, failed
    passed, failed = passed + bool(condition), failed + (not condition)
    print(("  PASS  " if condition else "  FAIL  ") + label, "" if condition else detail)


def scripted(*replies):
    queue = list(replies)

    def fake(messages, tools=None, model=None):
        assert queue, "the loop asked for more replies than the script has"
        return queue.pop(0)

    return fake


def reply(*calls):
    return {"text": None, "calls": [{"id": f"c{i}", "name": n, "args": a}
                                    for i, (n, a) in enumerate(calls)]}


FINISH = ("finish", {"reason": "nothing here would change a future run"})
PREFERENCE = ("save_fact", {
    "text": "Writes the C++ version first and rewrites it in Python for easy problems, and "
            "keeps tripping on reading the input. Wants conceptual hints only, not syntax.",
    "cue": ["python", "reading input", "hint style"], "scope": "private", "kind": "preference"})
SHARED_FACT = ("save_fact", {
    "text": "On 1729A the last window is the one people forget; the samples do not catch it.",
    "cue": ["1729a", "sliding window", "samples"], "scope": "shared", "kind": "context"})
RULE = ("propose_rule", {"line": "Never explain syntax unless the student asks.",
                         "rationale": "two runs where the student asked for ideas only"})

# %%
# The tests write to a sandbox, not to state/. Both are restored afterwards so the live cell
# at the bottom runs against the real stores.
REAL_STATE, REAL_RULES = db.STATE, TUTOR_RULES
sandbox = Path(tempfile.mkdtemp(prefix="curator-"))
db.STATE = sandbox / "state"
TUTOR_RULES = sandbox / "rules" / "tutor_rules.md"
TUTOR_RULES.parent.mkdir(parents=True)
TUTOR_RULES.write_text("# Tutor operating rules\n\n- Never show the repaired code.\n")

print("1. the model decides, and 'nothing' is a decision")
r = run_curator(NOISE_ENVELOPE, chat_fn=scripted(reply(FINISH)))
check("a run with only pleasantries saves nothing", r["status"] == "nothing_to_save", r)
check("...and the store is still empty", not db.facts_path().exists() or json.loads(
    db.facts_path().read_text()) == [])

r = run_curator(ENVELOPE, chat_fn=scripted(reply(PREFERENCE), reply(FINISH)))
check("a real preference is saved", r["status"] == "curated" and len(r["saved"]) == 1, r)
check("...with the cue the model wrote", "reading input" in r["saved"][0]["cue"])
check("...into the student's own scope", r["saved"][0]["scope"] == "user:u42")

print("\n2. the fact comes back on its cue, in a later run")
A, B = Session("u42"), Session("u77")
hits = retrieve_facts(A, "he sent python again and the input parsing is off")
check("a future request pulls it back", bool(hits) and "C++" in hits[0]["text"], hits)
check("...and it is rendered as data, not as an instruction",
      "not instructions" in render_facts(hits))
check("another student never sees it",
      retrieve_facts(B, "he sent python again and the input parsing is off") == [])

# %%
print("\n3. private and shared are not the same write")
r = run_curator(ENVELOPE, chat_fn=scripted(reply(SHARED_FACT), reply(FINISH)))
check("a fact about the problem goes to the shared scope", r["saved"][0]["scope"] == "shared", r)
check("...and reaches a different student", bool(retrieve_facts(B, "1729a sliding window")))

LEAK = ("save_fact", {"text": "u42 keeps messing up the input parsing on easy problems.",
                      "cue": ["input"], "scope": "shared", "kind": "fact"})
r = run_curator(ENVELOPE, chat_fn=scripted(reply(LEAK), reply(FINISH)))
check("a private detail cannot be published to everyone by naming the student",
      r["saved"] == [], r)
check("...and the refusal explains how to fix it",
      all("u42" not in d["text"] for d in retrieve_facts(B, "u42 input parsing easy")))

NO_CUE = ("save_fact", {"text": "Something worth knowing.", "cue": [], "scope": "private",
                        "kind": "fact"})
r = run_curator(ENVELOPE, chat_fn=scripted(reply(NO_CUE), reply(PREFERENCE), reply(FINISH)))
# PREFERENCE was already saved above, so it deduplicates and `saved` stays empty — what this
# pins is that the rejection came back as a tool result and the loop kept going.
check("a fact with no cue is rejected by the store, and the loop carries on",
      r["calls"] == 3 and r["status"] == "nothing_to_save", r)
check("...and nothing cue-less reached the store",
      all(d["cue"] for d in json.loads(db.facts_path().read_text())))

# %%
print("\n4. the gate is on the rule, not on the fact")
before = TUTOR_RULES.read_text()
r = run_curator(ENVELOPE, chat_fn=scripted(reply(RULE), reply(FINISH)), confirm=lambda q: False)
check("declining leaves the rules file untouched", TUTOR_RULES.read_text() == before)
check("...and the proposal is still recorded as declined",
      r["proposed"] == [{**RULE[1], "applied": False}], r["proposed"])

r = run_curator(ENVELOPE, chat_fn=scripted(reply(RULE), reply(FINISH)), confirm=lambda q: True)
check("an explicit yes applies it", "Never explain syntax" in TUTOR_RULES.read_text())
check("...under a Learned rules heading, with the run that proposed it",
      "## Learned rules" in TUTOR_RULES.read_text()
      and ENVELOPE["result"]["run_id"] in TUTOR_RULES.read_text())
check("only a literal 'yes' opens the gate",
      [is_yes(a) for a in ["yes", " YES ", "y", "yes please", "sure", ""]]
      == [True, True, False, False, False, False])

# %%
print("\n5. the branches that stop the loop")
r = run_curator(BROKEN_ENVELOPE, chat_fn=scripted())     # asserts if the model is consulted
check("a broken run is never learned from", r["status"] == "skipped_error_run", r)
check("...and costs zero model calls", r["calls"] == 0)

five = [reply(PREFERENCE)] * 5
r = run_curator(ENVELOPE, chat_fn=scripted(*five), budget=4)
check("the effort budget stops it", r["status"] == "budget_exhausted", r)
check("...at exactly the budget", r["calls"] == 4, r["calls"])

r = run_curator(ENVELOPE, chat_fn=scripted({"text": "I think we should save this.", "calls": []}))
check("prose is not a decision", r["status"] == "gave_up", r)

r = run_curator(ENVELOPE, chat_fn=scripted(reply(("save_fact", None)), reply(FINISH)))
check("unparseable arguments do not crash the run", r["status"] == "nothing_to_save", r)

# %%
print("\n6. the trail the two agents leave")
run_id = ENVELOPE["result"]["run_id"]
kinds = [e["kind"] for e in events.read_events(run_id=run_id)]
check("the hand-off is in the log", "handoff" in kinds)
check("...and in the table, with the executor as its source",
      events.handoffs_for(run_id)[0]["from_agent"] == "executor")
check("a rule proposal is a second hand-off, flagged as needing approval",
      any(h["to_agent"] == "admin" and h["needs_approval"] == 1
          for h in events.handoffs_for(run_id)))
check("every write is chained to the run that made it",
      all(e["run_id"] == run_id for e in events.read_events(run_id=run_id)))
check("the chain verifies after everything above", events.verify_chain()["ok"],
      events.verify_chain())

saved_ids = [e["payload"]["fact_id"] for e in events.read_events(kind="fact_saved")]
check("every saved fact is in the log", len(saved_ids) == len(set(saved_ids)) and saved_ids)

shutil.rmtree(sandbox)
db.STATE, TUTOR_RULES = REAL_STATE, REAL_RULES
print(f"\n{passed} passed, {failed} failed")

# %% [markdown]
# ## Live run
#
# Needs `LLM_BASE_URL` and `LLM_API_KEY`. Everything above runs without them.
#
# Two runs on purpose: the noisy one, where the right answer is to write nothing, and the real
# one. A curator that saves on both is not being careful, it is being agreeable.

# %%
# `__name__` is "__main__" in Jupyter and when run as a script, but not when another script
# imports this file — which is how trace_b.py reuses the loop without firing a second live run.
if __name__ == "__main__" and not OFFLINE:
    for label, env in [("noise", NOISE_ENVELOPE), ("real", ENVELOPE)]:
        r = run_curator(env, confirm=lambda q: (print(q), False)[1])
        print(f"\n--- {label}: {r['status']} in {r['calls']} call(s)")
        print("   ", r.get("reason", ""))
        for f in r["saved"]:
            print(f"    saved [{f['scope']}] {f['text']}\n      cue: {f['cue']}")
        for p in r["proposed"]:
            print(f"    proposed rule (applied={p['applied']}): {p['line']}")
else:
    print("offline — set LLM_API_KEY in .env for this cell")
