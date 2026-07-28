---
marp: true
paginate: true
footer: "DS314 · Session 1 — Coding Assistants as Agentic Systems"
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
  table { font-size: 0.8em; border-collapse: collapse; }
  th, td { border: 1px solid var(--rule); padding: 6px 12px; }
  th { background: var(--code-bg); }
  footer { color: var(--muted); font-size: 14px; }
  section::after { color: var(--muted); font-size: 14px; }
  section.lead { justify-content: center; text-align: left; }
  section.lead h1 { font-size: 54px; }
  section.lead h2 { border: none; color: var(--accent); }
  section.section { background: #12141c; justify-content: center; }
  section.section h1 { color: var(--accent); font-size: 44px; }
  section.demo { background: #14110c; }
  section.demo h2 { border-bottom-color: #4a3d1e; }
  section.demo h1 { color: #ffd479; }
  section.plan { font-size: 20px; }
  section.plan h2 { font-size: 30px; }
  section.plan ol { margin-top: 10px; }
  section.plan li { margin: 3px 0; }
---

<!-- _class: lead -->

# Session 1 — Coding Assistants as Agentic Systems

## How coding assistants work, the levers that improve them, and where the course goes

---

## Welcome to Agentic AI Systems

- DS314. Three weeks, fifteen sessions, three hours each.
- Two instructors: Sergey Cherepanov and Aleksandr Kuznetsov.

---

## Who is teaching

- **Sergey Cherepanov**
  - CTO at Gauss, a US fintech.
  - Backend architecture, functional programming, production agentic systems.
  - Haskell, Python, TypeScript.
- **Aleksandr Kuznetsov**
  - ML/AI architect and tech lead.
  - NLP, computer vision, and production ML across healthcare, pharma, and life sciences.
  - Python and Rust.

---

## The plan — fifteen sessions

- Two instructors: **Sergey** (Sessions 1–8), **Aleksandr** (Sessions 8–15).
- First half builds a working agent, 0 → MVP.
- Second half hardens it toward production.

---

## First half — build a working agent (Sergey)

1. Coding assistants — *today*
2. LLM as runtime
3. Tools & human-in-the-loop
4. Memory
5. Multi-agent systems & orchestration
6. Agents as products
7. Testing agentic systems

---

## Second half — harden toward production (Aleksandr)

9. Agent frameworks: LangChain, LangGraph, PydanticAI
10. RAG, hybrid search & reranking
11. Evaluation: RAG and agents
12. Observability: OpenTelemetry & MLflow
13. Advanced guardrails & safety
14. Production engineering & deployment

---

## What you leave with

- A multi-agent assistant you built and understand end to end.
- Habits for using coding assistants that outlast any single tool.

---

## What you need

- Python and the command line.
  - Some ML exposure helps and is not required.
- You have used a coding assistant.

---

## How you are graded

- Attendance: we expect you in the room.
- Homework: a sequence of assignments that build one project, so you practice everything together.
- Final demo: open only to students who completed the homework.

---

## Demo in the middle

- At the midpoint, you demo your first-half system end to end.
- Working flow, architecture, authority boundaries, failure handling.
- Then we hand over to Aleksandr for the second half.

---

## Questions before we start

- About the plan?
- About us?
- About you?

---

## By the end of today

- Get better results from your coding assistant this week, through concrete levers.
- Recognize how these agents fail, and why.
- See how today maps onto the rest of the course.

---

<!-- _class: section -->

# Part A — Levers you use this week

---

## Why coding first?

- Agentic assistants got good at coding before writing, spreadsheets, or law.
- Why coding, and not those? Guess before the reveal.

---

## Why coding is where these systems work first

- Tools are text: terminals, files, compilers, and tests all read and write it.
  - LLMs are text engines.

---

## Why coding is where these systems work first

- Tools are text: terminals, files, compilers, and tests all read and write it.
  - LLMs are text engines.
- Training data is saturated with code and shell.
  - so models call `ls`, `grep`, and friends fluently.

---

## Why coding is where these systems work first

- Tools are text: terminals, files, compilers, and tests all read and write it.
  - LLMs are text engines.
- Training data is saturated with code and shell.
  - so models call `ls`, `grep`, and friends fluently.
- Feedback is cheap and deterministic.
  - tests pass or fail, compilers succeed or error, files exist or not.

---

## The loop the course hangs on

- Read → edit → run → repeat.
  - and sometimes plan, observe, correct.
- Every lever we see today makes that loop run longer.

---

### Lever 1 · Close the loop

## Without automated feedback, you check every step

- No automated check:
  - you verify every step by hand.
- The slowest and most expensive way to run it.

---

## Close the loop

- The agent runs the loop on its own:
  - fails a test → reads the trace → edits → re-runs → passes.
- The automated feedback is what makes it agentic.

---

## One loop, a growing stack of test layers

- The same loop wraps layer after layer, each catching what the one below cannot:
  - unit and end-to-end tests
  - evals over the model's output
  - scripted UI tests that drive the product
  - agentic UI tests that explore it like a user
- Each layer up costs more and catches subtler bugs.

---

## Measure your own autonomy

- Count your messages against the agent's actions in a recent session.
- Typical today: ~1:5.
- The direction of travel: ~1:50.

---

## What the number is for

- It shows how often you are still the one checking each step.
- What raises it, across today's levers:
  - a plan file, a script library, subagents for noisy commands.

---

### Lever 2 · The 50% rule

## Context is a physical budget

- The window is a hard limit.
  - quality holds in roughly the first half.
- On a 300k window, stay under ~150k.

---

## Put the runner on a diet

- A failing run can dump thousands of log lines.
  - it buries the working context.
- Configure the runner:
  - pass or fail to stdout
  - each test's detail to its own log file

---

## The tells that you pushed too far

- Over-agreement:
  - "You're absolutely right" whether or not you are.
- Rules from your instructions dropped or half-applied.
- A claimed fix with no diff to show for it.

---

## The lever: know when to reset

- Restart once you are past the useful half.
- Carry a short summary forward, not the whole history.

---

### Lever 3 · Modularity

## An assistant loose in a big codebase overeats

- It follows imports A → B → C and fills the window before the real work.
- It finds three "Product" definitions in three modules and reads all three.
- Like a tourist at a buffet:
  - fills up on breadsticks before the main course.

---

## One task, one module

- The Axis of Change:
  - things that change together belong together.
- A well-cut task touches one module.
  - the assistant searches a bounded space.

---

## A module should fit in context

- Aim for a size the assistant holds at once (~60k–100k tokens).
- My own numbers:
  - clean modules: **50k–60k tokens**
  - muddied modules: **115k–160k tokens**
  - our legacy product: still millions

---

## Self-sufficient modules

- Own API, own logic, own data.
- Talk to other modules through contracts.
- Avoid cross-imports.
- "Could I extract this as a separate service?"

---

## The lever: bound the module

- Give the assistant a way to read a bounded module only.

---

### Lever 4 · Subagents as noise bins

## Noisy commands flood the context they run in

- `unit-test` in the main chat spends hundreds of lines per iteration.
- Even a trivial fix buries your working context.

---

## Subagents: disposable context

```
              ┌─────────────────────────────────────┐
              │          MAIN CONTEXT               │
              │                                     │
              │   Your task, your code, your plan   │
              │                                     │
              │         [clean & focused]           │
              └──────┬─────────┬─────────┬──────────┘
                     │         │         │
                  "✓ ok"  "✓ 2 fixed"  "✓ 3 rows"
                     │         │         │
              ┌──────┴───┐ ┌───┴────┐ ┌──┴───────┐
              │ compile  │ │  test  │ │ db query │
              │~~~~~~~~~~│ │~~~~~~~~│ │~~~~~~~~~~│
              │ 500 lines│ │ stack  │ │ failed   │
              │ warnings │ │ traces │ │ attempts │
              │ [noise]  │ │ [noise]│ │ [noise]  │
              └──────────┘ └────────┘ └──────────┘
               DISPOSABLE   DISPOSABLE  DISPOSABLE
```

---

## Delegate the noise to a subagent

- The subagent runs the command, returns only the result:
  - `✓ passed`, or `✗ failed` with a log path.
- The firehose stays in the subagent, never reaching your context.

---

## It cycles without you

- A command allowlist lets it retry without approval.
- You already do this alone; the subagent makes it explicit.

---

### Lever 5 · Condensation

## Chat history is not a state manager

- Raw history piles up noise:
  - dead ends, stack traces, failed attempts.
- Reloading it carries all of that into the next session.

---

## What to carry into the next chat

- Keep:
  - decisions made
  - invariants discovered
- Discard:
  - compile errors, failed attempts, stack traces

---

## The lever: a condensation workflow

- Write a short summary file at the end of a session.
  - seed the next one with it.
- More advanced: specs (e.g. "openspec") to track both requirements and progress.

---

## Every lever protects the main constraint: context

- close the loop
- reset at the halfway mark
- bound the module
- bin the noise
- carry over condensed progress

---

<!-- _class: section -->

# Part B — What this is, where it helps, where it goes

---

## Which of these is an agent?

- Eight systems, one at a time.
- Vote agent-or-not on each before we name any axis.

---

<!-- _class: lead -->

# A Telegram bot with fixed `/commands`

## Agent?

---

<!-- _class: lead -->

# A self-driving car

## Agent?

---

<!-- _class: lead -->

# Code autocomplete

## The grey suggestion text as you type. Agent?

---

<!-- _class: lead -->

# A voice assistant

## Siri, Alexa. Agent?

---

<!-- _class: lead -->

# A thermostat

## Agent?

---

<!-- _class: lead -->

# AlphaGo

## Agent?

---

<!-- _class: lead -->

# Claude Code

## Agent?

---

<!-- _class: lead -->

# ChatGPT Deep Research

## Agent?

---

## The axes behind your votes

- Tool use: none vs. many.

---

## The axes behind your votes

- Tool use: none vs. many.
- Control flow: fixed script vs. model chooses the next step.

---

## The axes behind your votes

- Tool use: none vs. many.
- Control flow: fixed script vs. model chooses the next step.
- Autonomy: you drive each step vs. the system drives.

---

## The axes behind your votes

- Tool use: none vs. many.
- Control flow: fixed script vs. model chooses the next step.
- Autonomy: you drive each step vs. the system drives.
- Language as the medium: reasons in text, acts through text.

---

## Where the line actually falls

- A thermostat loops forever and still is not an agent here:
  - fixed logic, no open tools.
- Agents need model-directed, open-ended control.

---

## What changed with LLMs?

- Before LLMs, how did automation work?
- What changed when LLMs arrived and got adopted?
- Discuss.

---

## Name the stages we passed

- From the earliest automation to today's agents.
- What are the distinct generations? You name them.

---

## What language adds

- The agent reasons in text and recovers from changes it never saw.
- The brittle flows never could.

---

## Where they shine, where they are a burden

- Shine:
  - greenfield code, scripts, PoCs
  - tests, PR review
  - bounded integrations, repetitive migrations
- Burden:
  - mature codebases with implicit invariants
  - concurrency and races
  - thorny debugging

---

## My own arc

- Year one on a mature backend: net-negative.
  - unpredictable timelines, hidden bugs, hours of debugging.
- Today most of the code I ship there is agent-written.

---

## What moved the number

- Better models helped, and so did the infrastructure around them.
- We cannot change the model but:
  - we can change the infrastructure around it.

---

## The bottleneck moved to verification

- Output used to be reliably mediocre.
  - now it is _unpredictably_ good.
- Verifying what the agent produced is where we work now.

---

## The hidden cost

- Delegating the typing also delegates the learning.
- For thorny bugs, by hand is often faster:
  - and it builds intuition the agent cannot give you.

---

## Own your code

- Review agent output as strictly as a human's.
  - same accountability.
- The commit carries a human name.
- A new release usually matters less than one boring shell script you write yourself.
- The engineering around the model is the leverage.

---

## The capabilities have names

- Feedback loop

---

## The capabilities have names

- Feedback loop
- Tool use

---

## The capabilities have names

- Feedback loop
- Tool use
- Context assembly

---

## The capabilities have names

- Feedback loop
- Tool use
- Context assembly
- Delegation

---

## The capabilities have names

- Feedback loop
- Tool use
- Context assembly
- Delegation
- Human-in-the-loop

---

## The capabilities have names

- Feedback loop
- Tool use
- Context assembly
- Delegation
- Human-in-the-loop
- Levels of autonomy

---

## They map onto the plan

- Feedback loop → the runtime and testing sessions.
- Context assembly → the memory session.
- Tool use and human-in-the-loop → the tools session.
- Delegation and levels of autonomy → the multi-agent session.
- and more...

---

## Three years out

- What will coding agents do then that they cannot today?
- What will still be hard?

---

## Two lines to carry

- "Code is cheap. What costs now is proof it works."
- "Delegating the work doesn't delegate the responsibility."

---

## Next session

- Session 2 — the LLM as an agent runtime.

---

## Homework — Build One Helper (due Session 2)

- ~30 min: make your coding assistant slightly better for your own workflow.
- Optional first step: measure your messages-to-actions ratio, then pick the change that raises it most.
- Pick one:
  - change one config
  - add one custom command (e.g. an end-of-chat condensation command)
  - write one `AGENTS.md` rule for your project
  - set up one subagent
- Submit a before / after: what was clunky, what changed, what you would still improve.

---

## Homework — Failure Bounty (due start of Session 6)

- Find a task where your coding assistant is measurably worse than you expected.
- Document:
  - the prompt (or prompts) you used
  - the output
  - your diagnosis: context, tools, planning, or verification
- The point is to make failure data, not shame — it seeds the diagnostic instinct the course is built on.
