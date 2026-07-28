# %% [markdown]
# # Loop 2 — turn the fix into a hint that does not give the answer away
#
# Loop 1 produced a repaired version of the student's program. The student must never see it.
# This notebook turns `(broken code, repaired code, diagnosis)` into a hint.
#
# The hard part is not writing hints, it is rejecting them. A model asked to be helpful drifts
# towards "on line 7, change `n - k` to `n - k + 1`", which teaches nothing. So every candidate
# is checked before it can be delivered, and the checker runs on a **different model family**
# from the writer — a model grading its own output agrees with itself far too often.
#
# Assignment checklist:
#
# * **tools** — `propose_hint`, `report_hint_verdict`, `deliver_hint` (enums, required fields)
# * **loop** — write -> check -> feed the criticism back -> write again
# * **stops** — approved, round cap, or the same hint twice
# * **gate** — `deliver_hint` asks in chat; a hint cannot be unseen and it spends one of the
#   student's hint allowances
# * **error branch** — if the checker does not answer in the expected shape, we **fail
#   closed**: no hint goes out at all

# %%
import json
import os
import re
import time
from pathlib import Path

from dotenv import load_dotenv
from openai import OpenAI

load_dotenv()

STATE = Path("state")
STATE.mkdir(exist_ok=True)
HINTS = STATE / "hints.jsonl"

OFFLINE = not os.getenv("LLM_API_KEY")
client = OpenAI(base_url=os.getenv("LLM_BASE_URL"), api_key=os.getenv("LLM_API_KEY") or "offline")
MODEL_WRITER = os.getenv("MODEL_HINTER", "anthropic/claude-haiku-4.5")
MODEL_CHECKER = os.getenv("MODEL_GUARDRAIL", "openai/gpt-5")   # a different vendor on purpose

# %% [markdown]
# ## Input
#
# Loop 1 writes `state/loop1_result.json`. If it is not there we use a fixture, so this
# notebook runs on its own.

# %%
FIXTURE = {
    "original": '''import sys
data = sys.stdin.read().split()
n, k = int(data[0]), int(data[1])
a = [int(x) for x in data[2:2 + n]]
window = sum(a[:k])
best = window
for i in range(1, n - k):
    window += a[i + k - 1] - a[i - 1]
    best = max(best, window)
print(best)
''',
    "diagnosis": "The sliding window loop stops one iteration early, so the last window is never scored.",
    "tag": "off_by_one",
}
FIXTURE["code"] = FIXTURE["original"].replace("range(1, n - k)", "range(1, n - k + 1)")

handoff = STATE / "loop1_result.json"
CASE = json.loads(handoff.read_text()) if handoff.exists() else FIXTURE
USER_ID, PROBLEM_ID = "u42", "1729A"
print("input from", "loop 1" if handoff.exists() else "fixture")

# %% [markdown]
# ## Tools

# %%
CONCEPTS = ["loop_bounds", "initialization", "data_type", "edge_case", "io_format", "algorithm"]

WRITER_TOOLS = [
    {"type": "function", "function": {
        "name": "propose_hint",
        "description": "Submit a candidate hint for review. Call it once you know which single "
                       "concept the student is missing. Do not include code, line numbers, or "
                       "'change X to Y' — a hint that can be applied without thinking gets "
                       "rejected and costs you a round.",
        "parameters": {
            "type": "object",
            "properties": {
                "hint_text": {"type": "string", "description": "1-3 sentences, addressed to the student."},
                "targets_concept": {"type": "string", "enum": CONCEPTS},
            },
            "required": ["hint_text", "targets_concept"],
        },
    }},
    {"type": "function", "function": {
        "name": "deliver_hint",
        "description": "Send an approved hint to the student. This cannot be undone and it "
                       "spends one of their hint allowances, so a human confirms it. Call it "
                       "only after the checker approved the hint.",
        "parameters": {
            "type": "object",
            "properties": {"confirm_text": {"type": "string", "description": "The exact approved hint."}},
            "required": ["confirm_text"],
        },
    }},
]

CHECKER_TOOLS = [
    {"type": "function", "function": {
        "name": "report_hint_verdict",
        "description": "Report your judgement on one candidate hint. This is your only way to "
                       "answer — do not reply in prose.",
        "parameters": {
            "type": "object",
            "properties": {
                "verdict": {"type": "string", "enum": ["approve", "too_explicit", "off_target"]},
                "directness": {"type": "integer", "minimum": 0, "maximum": 100,
                               "description": "How much the hint gives away."},
                "feedback": {"type": "string", "description": "One sentence the writer can act on."},
            },
            "required": ["verdict", "directness", "feedback"],
        },
    }},
]

# %% [markdown]
# ## Check 1: rules
#
# Cheap and deterministic, so a hint that was never going to pass costs us nothing. These are
# the leaks a model does not get a vote on.
#
# We look for whole repaired lines *and* the call expressions inside them — the first version
# only checked whole lines, and "use `range(1, n - k + 1)` instead" sailed straight through.
# Anything the student already wrote is excluded: quoting `sys.stdin.read()` back at them
# reveals nothing.

# %%
CALL_EXPR = re.compile(r"[A-Za-z_][\w.]*\s*\((?:[^()]|\([^()]*\))*\)")
LINE_REF = re.compile(r"\b(line|lines|строк\w*)\s*#?\s*\d+", re.IGNORECASE)
PRESCRIBES = re.compile(r"\b(replace|change|swap)\b[^.]{0,60}\b(with|to|for)\b", re.IGNORECASE)
CODE_SPAN = re.compile(r"```|`[^`]{6,}`")


def flat(text):
    return re.sub(r"\s+", " ", text).strip()


def leak_fragments(original, fixed):
    old = original.splitlines()
    out = []
    for line in fixed.splitlines():
        if line in old or not line.strip():
            continue
        for candidate in [line, *CALL_EXPR.findall(line)]:
            frag = flat(candidate)
            if len(frag) >= 10 and frag not in flat(original) and frag not in out:
                out.append(frag)
    return out


def looks_explicit(hint, original, fixed):
    """Returns the rules that fired. Empty means nothing provably leaked."""
    reasons = []
    for frag in leak_fragments(original, fixed):
        if frag in flat(hint):
            reasons.append(f"quotes the repaired code: {frag!r}")
    if LINE_REF.search(hint):
        reasons.append("points at a line number")
    if CODE_SPAN.search(hint):
        reasons.append("contains a code span")
    if PRESCRIBES.search(hint):
        reasons.append("prescribes the edit")
    return reasons

# %% [markdown]
# ## Check 2: the second model
#
# Only for hints that survive the rules, because "too explicit" is a judgement call while
# "you quoted the answer" is not.
#
# The checker answers by calling `report_hint_verdict`. If it does not — prose, silence, a
# broken connection — that is **not** an approval and not a rejection, it is an error, and
# `judge` returns `ok=False`. The failure mode being designed against is a checker that
# returns something we cannot read and a loop that shrugs and delivers the hint anyway.

# %%
CHECKER_PROMPT = """You review hints written for a student debugging their own program.

You get the student's broken code, the repaired code, and a candidate hint. The student will
see ONLY the hint.

Reject it if a student could apply it mechanically without understanding the defect: it states
the corrected expression, names the exact edit, or points at a line. Approve it if it makes
them re-examine the right part of their own reasoning. A hint that is merely vague is
off_target, not too_explicit.

Answer by calling report_hint_verdict."""


def chat(messages, tools, model):
    r = client.chat.completions.create(model=model, messages=messages,
                                       tools=tools, tool_choice="auto", temperature=0.4)
    m = r.choices[0].message
    calls = []
    for tc in m.tool_calls or []:
        try:
            args = json.loads(tc.function.arguments)
        except json.JSONDecodeError:
            args = None
        calls.append({"id": tc.id, "name": tc.function.name, "args": args})
    return {"text": m.content, "calls": calls}


def judge(hint, original, fixed, chat_fn):
    reasons = looks_explicit(hint, original, fixed)
    if reasons:
        return {"ok": True, "by": "rules", "verdict": "too_explicit", "directness": 100,
                "feedback": "Remove the quoted code, the line reference and any 'change X to Y' "
                            "phrasing. Ask about the property that is wrong instead. "
                            f"({reasons[0]})"}
    try:
        out = chat_fn([{"role": "system", "content": CHECKER_PROMPT},
                       {"role": "user", "content": f"BROKEN\n{original}\n\nREPAIRED\n{fixed}\n\n"
                                                   f"CANDIDATE HINT\n{hint}"}],
                      CHECKER_TOOLS, MODEL_CHECKER)
    except Exception as exc:
        return {"ok": False, "error": f"checker unreachable: {type(exc).__name__}"}

    call = next((c for c in out["calls"] if c["name"] == "report_hint_verdict"), None)
    if call is None or call["args"] is None:
        return {"ok": False, "error": "checker did not call report_hint_verdict"}
    if call["args"].get("verdict") not in ("approve", "too_explicit", "off_target"):
        return {"ok": False, "error": f"unknown verdict {call['args'].get('verdict')!r}"}
    return {"ok": True, "by": "model", **call["args"]}

# %% [markdown]
# ## The loop
#
# | stop | status |
# |---|---|
# | approved, then a confirmed send | `delivered` |
# | approved, human declined the send | `approved_not_delivered` |
# | round cap | `exhausted` |
# | the same hint twice — the criticism is not landing | `stalled` |
# | the checker could not be read | `checker_failed` |
#
# Only the first two produce a hint. Every other exit delivers nothing, on purpose.

# %%
WRITER_PROMPT = """You write hints for students debugging their own competitive-programming
submissions.

You get their broken program, a repaired one that passes every test, and a diagnosis. The
student sees ONLY your hint — never the repaired code.

A good hint names the property that is wrong and makes them check it: "your loop covers every
window except one — which one?" A bad hint is anything they can apply without thinking.

Call propose_hint. If it is rejected, do not reword it — aim at a different level of
abstraction. Once it is approved, call deliver_hint."""


def is_yes(answer):
    return answer.strip().lower() == "yes"


def ask_human(question):
    return is_yes(input(f"{question}\nType 'yes' to send it: "))


def save_hint(user_id, problem_id, hint, concept):
    STATE.mkdir(exist_ok=True)   # the directory can be gone by now; recreate it at write time
    with HINTS.open("a") as fh:
        fh.write(json.dumps({"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                             "user_id": user_id, "problem_id": problem_id,
                             "hint": hint, "concept": concept}) + "\n")


def run_hint_loop(case, writer_fn, checker_fn, max_rounds=3, confirm=ask_human):
    messages = [
        {"role": "system", "content": WRITER_PROMPT},
        {"role": "user", "content": f"Diagnosis: {case['diagnosis']}\nMistake class: {case['tag']}\n\n"
                                    f"BROKEN\n{case['original']}\n\nREPAIRED (never quote this)\n{case['code']}"},
    ]
    seen, approved, rejected, rounds = set(), None, [], 0

    while rounds < max_rounds:
        out = writer_fn(messages, WRITER_TOOLS, MODEL_WRITER)
        if not out["calls"]:
            return {"status": "gave_up", "hint": None, "rejected": rejected}

        messages.append({"role": "assistant", "content": out["text"], "tool_calls": [
            {"id": c["id"], "type": "function",
             "function": {"name": c["name"], "arguments": json.dumps(c["args"])}}
            for c in out["calls"]]})

        for c in out["calls"]:
            def answer(payload):
                messages.append({"role": "tool", "tool_call_id": c["id"], "content": json.dumps(payload)})

            if c["name"] == "deliver_hint":
                if approved is None:
                    answer({"ok": False, "error": "no hint has been approved yet"})
                    continue
                if not confirm(f"Send this hint to {USER_ID}? It cannot be unsent and it spends "
                               f"one hint allowance.\n\n  {approved['hint']}\n"):
                    return {"status": "approved_not_delivered", "hint": approved["hint"], "rejected": rejected}
                save_hint(USER_ID, PROBLEM_ID, approved["hint"], approved["concept"])
                return {"status": "delivered", "hint": approved["hint"], "rejected": rejected}

            if c["name"] != "propose_hint" or c["args"] is None:
                answer({"ok": False, "error": "call propose_hint with hint_text and targets_concept"})
                continue

            hint = c["args"]["hint_text"]
            if hint in seen:
                return {"status": "stalled", "hint": None, "rejected": rejected}
            seen.add(hint)
            rounds += 1

            verdict = judge(hint, case["original"], case["code"], checker_fn)
            if not verdict["ok"]:
                # Fail closed. An unreadable checker is not an approval.
                return {"status": "checker_failed", "hint": None,
                        "error": verdict["error"], "rejected": rejected}

            if verdict["verdict"] == "approve":
                approved = {"hint": hint, "concept": c["args"]["targets_concept"]}
                answer({"ok": True, "verdict": "approve", "note": "approved — call deliver_hint"})
            else:
                rejected.append({"hint": hint, "directness": verdict["directness"],
                                 "by": verdict["by"], "feedback": verdict["feedback"]})
                answer({"ok": True, "verdict": verdict["verdict"],
                        "directness": verdict["directness"], "feedback": verdict["feedback"],
                        "rounds_left": max_rounds - rounds})

    return {"status": "exhausted", "hint": None, "rejected": rejected}

# %% [markdown]
# ## Tests
#
# The writer and the checker are scripted **separately**. That is the point of splitting them:
# a test can hold the writer steady and break only the checker.

# %%
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


def hint_reply(text, concept="loop_bounds"):
    return {"text": None, "calls": [{"id": "h", "name": "propose_hint",
                                     "args": {"hint_text": text, "targets_concept": concept}}]}


DELIVER = {"text": None, "calls": [{"id": "d", "name": "deliver_hint", "args": {"confirm_text": "ok"}}]}


def verdict_reply(verdict, directness):
    return {"text": None, "calls": [{"id": "v", "name": "report_hint_verdict",
                                     "args": {"verdict": verdict, "directness": directness,
                                              "feedback": "be less specific"}}]}


GOOD = "Your loop covers every window but one. Which window never gets scored, and when would that change the answer?"
LEAKY = "In the for statement, use range(1, n - k + 1) instead of what you have."

# %%
print("1. the rules, both directions")
for label, text, should_fire in [
    ("quotes the repaired expression", LEAKY, True),
    ("names a line number", "Look at line 7 of your solution.", True),
    ("prescribes the edit", "You should change the loop bound to cover one more window.", True),
    ("contains a code span", "Try `range(1, n - k + 1)` there.", True),
    ("a socratic hint passes", GOOD, False),
    ("concept talk passes", "Think about which elements your sliding window never reaches.", False),
]:
    check(label, bool(looks_explicit(text, CASE["original"], CASE["code"])) == should_fire)

never = scripted()   # raises if consulted
check("a rule-caught leak costs zero checker calls",
      judge(LEAKY, CASE["original"], CASE["code"], never)["by"] == "rules")

# %%
print("\n2. happy path: rejected once, approved, delivered")
HINTS.unlink(missing_ok=True)
r = run_hint_loop(CASE,
                  writer_fn=scripted(hint_reply("Your loop finishes one step early near the end."),
                                     hint_reply(GOOD), DELIVER),
                  checker_fn=scripted(verdict_reply("too_explicit", 70), verdict_reply("approve", 20)),
                  confirm=lambda q: True)
check("status is delivered", r["status"] == "delivered", r["status"])
check("the delivered hint is the second one", r["hint"] == GOOD)
check("the rejected one is kept for review", len(r["rejected"]) == 1 and r["rejected"][0]["by"] == "model")
check("exactly one hint was recorded", HINTS.read_text().count("\n") == 1)

print("\n3. stopping conditions, and neither delivers a hint")
r = run_hint_loop(CASE, writer_fn=scripted(hint_reply("a"), hint_reply("b")),
                  checker_fn=scripted(verdict_reply("too_explicit", 80), verdict_reply("too_explicit", 78)),
                  max_rounds=2)
check("the round cap stops the loop", (r["status"], r["hint"]) == ("exhausted", None), r["status"])

r = run_hint_loop(CASE, writer_fn=scripted(hint_reply("same"), hint_reply("same")),
                  checker_fn=scripted(verdict_reply("too_explicit", 80)), max_rounds=9)
check("the same hint twice stops the loop", (r["status"], r["hint"]) == ("stalled", None), r["status"])

# %%
print("\n4. the checker breaks — fail closed")


def broken_connection(messages, tools=None, model=None):
    raise ConnectionError("gateway 502")


for label, checker in [
    ("prose instead of a tool call", scripted({"text": "Looks great to me!", "calls": []})),
    ("wrong tool", scripted({"text": None, "calls": [{"id": "v", "name": "something_else", "args": {}}]})),
    ("verdict outside the enum", scripted({"text": None, "calls": [
        {"id": "v", "name": "report_hint_verdict",
         "args": {"verdict": "looks_fine", "directness": 5, "feedback": ""}}]})),
    ("unparseable arguments", scripted({"text": None, "calls": [
        {"id": "v", "name": "report_hint_verdict", "args": None}]})),
    ("connection error", broken_connection),
]:
    r = run_hint_loop(CASE, writer_fn=scripted(hint_reply(GOOD)), checker_fn=checker)
    check(f"{label} -> checker_failed, nothing delivered",
          (r["status"], r["hint"]) == ("checker_failed", None), r["status"])

# A fail-closed check that fails on everything is just an outage, so test the inverse too.
r = run_hint_loop(CASE, writer_fn=scripted(hint_reply(GOOD), DELIVER),
                  checker_fn=scripted(verdict_reply("approve", 15)), confirm=lambda q: True)
check("a healthy checker still approves", r["status"] == "delivered", r["status"])

# %%
print("\n5. the approval gate")
before = HINTS.read_text().count("\n")
r = run_hint_loop(CASE, writer_fn=scripted(hint_reply(GOOD), DELIVER),
                  checker_fn=scripted(verdict_reply("approve", 15)), confirm=lambda q: False)
check("declining holds the hint", r["status"] == "approved_not_delivered", r["status"])
check("nothing was recorded", HINTS.read_text().count("\n") == before)
check("only a literal 'yes' opens the gate",
      [is_yes(a) for a in ["yes", " YES ", "y", "yes please", "sure", ""]]
      == [True, True, False, False, False, False])

print(f"\n{passed} passed, {failed} failed")

# %% [markdown]
# ## Live run

# %%
if not OFFLINE:
    r = run_hint_loop(CASE, writer_fn=chat, checker_fn=chat)
    print(r["status"], "\n\nhint:", r["hint"])
    for bad in r["rejected"]:
        print(f"\nrejected by {bad['by']} (directness {bad['directness']}): {bad['hint']}\n  -> {bad['feedback']}")
else:
    print("offline — set LLM_API_KEY in .env for this cell")
