---
marp: true
paginate: true
footer: "DS314 · Session 2 — The Raw LLM API"
style: |
  :root {
    --bg: #0f1117;
    --fg: #e8e8ea;
    --muted: #9aa0aa;
    --accent: #6ea8fe;
    --rule: #2a2f3a;
    --code-bg: #171a22;
  }
  section {
    background: var(--bg);
    color: var(--fg);
    font-family: -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    font-size: 25px;
    line-height: 1.42;
    padding: 56px 64px;
  }
  h1 { font-size: 46px; color: var(--fg); font-weight: 700; letter-spacing: -0.5px; }
  h2 { font-size: 33px; color: var(--fg); font-weight: 650; border-bottom: 2px solid var(--rule); padding-bottom: 10px; margin-bottom: 20px; }
  h3 { font-size: 22px; color: var(--accent); font-weight: 600; text-transform: uppercase; letter-spacing: 1px; margin-bottom: 4px; }
  strong { color: #ffd479; font-weight: 650; }
  a { color: var(--accent); }
  code { background: var(--code-bg); color: #c3e88d; padding: 1px 6px; border-radius: 4px; font-size: 0.86em; font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; }
  pre { background: var(--code-bg); border: 1px solid var(--rule); border-radius: 8px; padding: 16px 18px; font-size: 0.72em; line-height: 1.4; font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; }
  ul, ol { margin-top: 6px; }
  li { margin: 5px 0; }
  blockquote { border-left: 3px solid var(--accent); color: var(--muted); padding-left: 16px; font-style: normal; }
  footer { color: var(--muted); font-size: 14px; }
  section::after { color: var(--muted); font-size: 14px; }
  section.lead { justify-content: center; text-align: left; }
  section.lead h1 { font-size: 54px; }
  section.lead h2 { border: none; color: var(--accent); }
  section.section { background: #12141c; justify-content: center; text-align: center; }
  section.section h1 { color: var(--accent); font-size: 60px; }
  section.section h2 { border: none; color: var(--muted); font-weight: 500; }
  section.poster { background: #000; justify-content: center; text-align: center; }
  section.poster p { margin: 0; text-align: center; }
  section.poster img { border-radius: 6px; box-shadow: 0 8px 40px rgba(0,0,0,0.6); }
  section.handoff { background: #12141c; justify-content: center; text-align: center; }
  section.handoff h1 { color: #ffd479; font-size: 48px; }
---

<!-- _class: lead -->
<!-- _paginate: false -->

# Session 2 — The Raw LLM API

## How to use brains — what one model call does well, where it fails, and how we configure this

---

<!-- _class: section -->
<!-- _paginate: false -->

# Let's play a game

## First, the ground rules...

---

<!-- _class: poster -->
<!-- _paginate: false -->

![w:560](onebuzzwordafteranother.jpeg)

---

## Yesterday a part of the definition of an **agent** was:

> a system with a brain we can never fully inspect.

---

## Today: the layer closest to that brain

- A single model call — the layer **closest** to the brain.
- The rest of the course walks steadily away from it: tools, memory, orchestration.
- We see its weaknesses at short distance.

**No "agent" yet.**

---

## Today's shape

- Run a one-shot → **hit a wall** → name the fix.
- Where it's reliable: summarise, paraphrase, extract.
- Where it fails: following constraints, counting/arithmetic, fresh facts.
- The knobs you own: temperature, structured output, the token bill, model routing.

---

<!-- _class: handoff -->

# Now we switch to the notebooks

## `Part_A_the_callable.ipynb`

---

<!-- _class: section -->
<!-- _paginate: false -->

# Back to slides

## The request and the response

---

## One request in, one response out

- Every call so far: one request in, one response out.
- They are independent — the model keeps **nothing** between them.
- To continue, you **resend the messages yourself**.

---

## The request

```
model         "gpt-5.4"
input         [ messages ]     role: system | user, content: text
temperature   0.0 … 2.0        higher = more varied
text.format   text | json_object | json_schema
```

---

## The response

```
output_text   the reply
usage         input_tokens · output_tokens · cached_tokens
```

---

## Next — make it think, and choose

- Reason through a hard step.
- Structure the output.
- Pick the model per message.

---

<!-- _class: handoff -->

# Back to the notebook

## `Part_B_remember_think_choose.ipynb`

---

<!-- _class: section -->
<!-- _paginate: false -->

# Closing

## The same objects again

---

## The request grew

```
+ reasoning = {effort: none…xhigh, summary: "auto"}   the effort dial
+ max_output_tokens                                    caps the answer
```

- Everything from Part A still applies.

---

## The response grew

```
+ status: completed | incomplete
+ incomplete_details.reason     why it stopped, e.g. max_output_tokens
+ usage.reasoning_tokens
+ a reasoning item in output    returned as a summary
```

---

## `output` is a list of items

```
message               your turns and the model's reply
reasoning             the thinking — hidden, returned as a summary
function_call         the model asks to call a tool     ← next session
function_call_output  you return the tool's result      ← next session
```

---

## Tools — you'll meet them next session

```
tools = [ {type:"function", name, description, parameters=JSON Schema} ]
```

- It's a **schema**, the same object as your structured output.

---

## Everything else — low-level, optional

```
stream · store · text.verbosity · previous_response_id · conversation
include · background · parallel_tool_calls · metadata · top_p · truncation
```

- May be useful one day, today none is required to build the course project.

---

## Today: a brain with no hands

- You already know how to write the schema.
- Next session the callable crosses the line — it becomes an **agent**.

---

## Debate — build for today's model, or tomorrow's?

- Altman to AI founders: 
  - assume the models keep improving.
  - if the next model makes your product obsolete, OpenAI will steamroll it.
- Does it make sense to build an AI product?
  - OpenAI and others kill hundreds or thousands of them every year.

---

<!-- _class: section -->
<!-- _paginate: false -->

# Homework

---

## Homework — none _graded_ this session

- The course project starts **next session**.
- Due today: HW1 "Build One Helper" (from Session 1).
- Still running: the week-long **Failure Bounty** (due Session 6).

---

## Optional take-home — Playground (not graded)

**Router gauntlet** — three prompts that fool your router:
- one that looks easy but the cheap model botches;
- one where the cheap model nails a "hard" one;
- one where chain-of-thought flips the answer.

**Dialing** — push temperature, structured-output level, caching, router threshold until something surprises you.

---

<!-- _class: handoff -->

