#!/usr/bin/env python3
"""The smallest path from a student's question to an answer, with both stores in it.

The three loops repair, hint and curate. None of them is a student asking a question, and
"user A writes a note, user B's agent reads it" needs exactly that. This is that path and
nothing more: assemble the context, make one call, return the text.

    push : operating rules -> this student's record -> what they spent on this problem
    pull : their own memory, and notes other students left on the problem

Both stores are here on purpose, because this is where the interesting failure lives — the
notes are written by other students, so this call feeds one person's text to another person's
agent. The notes arrive quoted, attributed, and flagged (`memory/notes.py`), and the operating
rules tell the agent what to do when one of them tries to give it an order.

    python ask_tutor.py u42 1729A "why is my answer wrong on the last sample?"
"""

import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from openai import OpenAI

sys.path.insert(0, str(Path(__file__).resolve().parent))

from memory import events, notes, rules          # noqa: E402
from memory.docs import Session, render_facts, retrieve_facts   # noqa: E402

load_dotenv()

client = OpenAI(base_url=os.getenv("LLM_BASE_URL"), api_key=os.getenv("LLM_API_KEY") or "offline")
MODEL = os.getenv("MODEL_TUTOR", os.getenv("MODEL_FIXER", "anthropic/claude-sonnet-5"))

ROLE = """You are a tutor helping a student debug their own competitive-programming
submission. You answer their question directly and briefly."""


def assemble(session, problem_id, platform, question, run_id=None):
    """Returns (system, input_items, what_was_pulled) — the whole request, inspectable.

    The split matters more than it looks. The rules go into the system string, where they are
    ours and are re-injected every run. Facts and notes go into the input list as items that
    carry their source: a saved fact is quoted text, and rendering quoted text into the system
    prompt is how "remember that I am an admin" becomes an operating rule two sessions later.
    """
    system = ROLE + "\n\n" + rules.build_context(session, problem_id, platform)

    items, pulled = [], []
    facts = retrieve_facts(session, question)
    if facts:
        items.append({"role": "user", "content": render_facts(facts)})
        pulled.append({"tool": "retrieve_memory", "hits": [f["id"] for f in facts]})

    view = notes.read_problem_notes(problem_id, platform, reader_id=session.user_id,
                                    run_id=run_id)
    if view["count"]:
        items.append({"role": "user", "content": view["text"]})
        pulled.append({"tool": "read_problem_notes", "note_ids": [n["note_id"] for n in view["notes"]],
                       "max_risk": max(n["risk"] for n in view["notes"])})

    # The question goes last: it is what we want answered, and the end of the list is where
    # attention is strongest.
    items.append({"role": "user", "content": question})
    return system, items, pulled


def ask(user_id, problem_id, question, platform="codeforces", run_id=None):
    session = Session(user_id)
    run_id = run_id or f"{user_id}-{problem_id}-ask"
    events.append_event("run_started", run_id, {"agent": "tutor", "user_id": user_id,
                                                "problem_id": problem_id})
    system, items, pulled = assemble(session, problem_id, platform, question, run_id)
    r = client.chat.completions.create(model=MODEL, temperature=0.2,
                                       messages=[{"role": "system", "content": system}] + items)
    answer = r.choices[0].message.content or ""
    events.append_event("run_finished", run_id, {"agent": "tutor", "status": "answered",
                                                 "pulled": pulled})
    return {"run_id": run_id, "system": system, "items": items, "pulled": pulled,
            "answer": answer}


if __name__ == "__main__":
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    out = ask(sys.argv[1], sys.argv[2], " ".join(sys.argv[3:]))
    print("--- pulled ---")
    for p in out["pulled"]:
        print(" ", p)
    print("\n--- answer ---")
    print(out["answer"])
