---
marp: true
paginate: true
footer: "DS314 · Session 5 — Multi-Agent & Orchestration"
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
  h3 { font-size: 27px; color: var(--accent); font-weight: 600; }
  strong { color: #ffd479; font-weight: 650; }
  a { color: var(--accent); }
  code { background: var(--code-bg); color: #c3e88d; padding: 1px 6px; border-radius: 4px; font-size: 0.86em; }
  pre { background: var(--code-bg); border: 1px solid var(--rule); border-radius: 8px; padding: 16px 18px; font-size: 0.72em; line-height: 1.4; }
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
  section.poster { background: #000; justify-content: center; text-align: center; }
  section.poster p { margin: 0; text-align: center; }
  section.poster img { border-radius: 6px; box-shadow: 0 8px 40px rgba(0,0,0,0.6); }
---

<!-- _class: lead -->

# Session 5 — Multi-Agent & Orchestration

## When a second agent earns its cost, and how to stay in control of a team of them

---

## By the end of today

- Name the concrete goal a second agent buys before adding one.
- Walk a task up a complexity ladder and pick the simplest architecture that works.
- Cut a system into agents by domain slice, not by job title.
- Know where a critic helps and where more agents make results worse.
- Write an agent's delegation brief and place it on a levels-of-autonomy scale.

---

<!-- _class: section -->

# Part A — Should you?

---

## Sixteen agents built a working C compiler

- ~2B tokens in, ~140M out, just under $20,000.
- A custom harness of parallel agents, no orchestrating agent.
- Other examples:
  - Bun's recent rewrite from Zig to Rust
  - FastRender (Cursor Browser Experiment)
  - and more

---

## "Multi-agent" covers very different systems

- A doc-analysis pipeline with fixed steps is multi-agent.
- The compiler team above is multi-agent.
- They share almost no design decisions.

---

## Locate your system on the range first

- When a team pays off versus when one agent is enough.
- Fixed end: who runs next, what each hands off.
- Long-running end: how they coordinate, recover, stay consistent.

---

## A second agent usually gets added on a feeling

- The reasons given: cleaner, more capable, more like a team.
- A second agent costs tokens and complexity.
- Faced with a hard task, you think "I'll add an agent."
  - Now you have two agents and three problems.

---

## Parallelism: one agent is too slow

- Independent chunks, no dependency between them.
- Read five course modules at once, not one after another.
- Buys wall-clock time.

---

## Context economy: the window fills with noise

- A subtask dumps a huge log or search result.
- A subagent reads it all, returns the few lines that matter.
- The main window stays clean.

---

## Focus: several goals distract

- One small email, three concerns at once.
- Read it separately: a legal lens, an accounting lens, a sales lens.
- Each lens stays sharp, even when it saves no tokens.

---

## Decoupled execution: work on different clocks

- Some work is live with the user; some waits for a batch.
- Live helpers now; an overnight agent reviews the day.
- Each fires on its own trigger.

---

## Name the payoff before you split

- Say which of the four you are buying.
- No nameable payoff means one agent.

---

## We design a team before we need one

- One agent is the default.
- Adding agents up front pays the cost before earning it.
- The first design you think of is usually over-built.

---

## The ladder: a Harbour.Space study assistant

- An assistant for students getting through an intensive module.
- Each rung, predict the architecture before the reveal.

---

## Rung 0: "answer my questions about this course"

- One agent over the course material. No split.

---

## Rung 1: "check my assignment against the criteria"

- A doer drafts; a checker judges against the rubric.
- Earns the split only because the rubric is real.

---

## Rung 2: "gather everything due this week, three classes"

- Material scattered across classes, each a long read.
- One subagent per class, each returns a short list.

---

## Rung 3: "the assistant misleads students — find where"

- Overnight, an agent reads the day's conversations.
- Flags syllabus contradictions and common sticking points.
- A monitor on its own clock, above the live helpers.

---

## Rung 4: a "check the homework" agent

- One runs tests, one reviews code, one checks it matches the task.
- The class and its assignment are the same for every student.

---

## Rung 4: one agent or several?

- Does the split buy any of the four payoffs?
- Only small, optional differences in how they worked, nothing critical to catch.
- Shared context is large and the stakes are low, so one agent does it.

---

## Durable principle

**Every extra agent adds capability but multiplies the ways the system can fail.**

- Each added agent brings one agent's worth of capability.
- Failure modes and costs multiply:
  - interfaces, tokens, debugging time
- Sometimes the multiplied price outweighs the added gain.
- The default is one agent; each split names its payoff.

---

## "Add a reviewer agent" is treated as free quality

- Tempting: a second agent reviews the first.
- It helps sometimes and adds noise other times.
- The difference is whether it can check something real.

---

## A good yardstick is specific and checkable

- Weak: "rate this 1 to 10." The critic guesses.
- Strong, for the assignment checker:
  - Does it pass the provided tests? (run them)
  - Is each brief requirement addressed?
  - Are the named edge cases handled?
  - Any claims the brief never asked for?

---

## The loop helps far more with a real yardstick

- With the loop, it catches misses a single agent ships.
- It costs extra rounds and does not always improve things. Cap them.
- Without one, the critic adds confident but wrong feedback.

---

## Are more agents more reliable?

- Voting, debate, a committee.


---

<!-- _class: poster -->
<!-- _paginate: false -->

![w:760](design-by-committee.png)

---

## Committee output lands on the bland option

- A group must agree on one design.
- Every bold idea draws a risk objection.
- The result converges on the plainest, safest choice.
- Forced agreement converges on the mediocre.
- Cursor's flat agent swarm did the same: with no lead, agents turned risk-averse and left the hard tasks undone.

---

## Copies of one model fail together

- One is right, two confidently wrong, the right one caves.
- The same model has the same blind spots.
- Ten agreeing carry about as much as two or three.

> *Nine Judges, Two Effective Votes* — nine different models, ≈2 effective votes.

---

## What raises reliability

- Two different, grounded agents beat many identical ones.
- Vary the model, the prompt, and the angle.
- Give each one a real check.

---

## You secure each agent and assume the system is safe

- You lock down each agent.
- The whole can still have a hole.
- The hole lives in the combination.

---

## The lethal trifecta, spread across agents

- Take untrusted content.
- Read private data.
- Send outside.

- Even when each agent is safe, the pipeline across them can still exfiltrate.

---

## Two safe agents, one leak

The study assistant, split in two, sharing the Session 4 memory:

- **Chat helper** — reads student messages, writes notes to shared memory. Sends nothing out.
- **Digest agent** — reads shared memory, emails staff a weekly summary.
- Each agent holds at most two legs. Neither looks dangerous alone.

---

## Find the exfiltration path

- A student pastes instructions into an ordinary chat message.
- The chat helper stores them as a note, as it stores everything.
- The digest agent reads that note and treats it as an instruction.
- Pairs: which agent holds which leg, and where is the cheapest cut?

---

## A subagent inherits what you forgot to remove

- In many stacks it runs with the orchestrator's permissions by default.
- A poisoned tool result steers the next agent.

---

## Secure by removing capability

- Take tools away instead of adding rules that ask it to behave.
- Splitting helps: fewer tools each, few critical agents to watch.
- Point your limited review at the dangerous ones.

---

## The attack that waits

- One agent writes now; another reads weeks later.
- A poisoned entry sits dormant, then fires.
- Treat cross-agent writes as untrusted. Keep an audit trail.

---

<!-- _class: section -->

# Part B — How to build it

---

## The reflex split is by job title

- A dev task, so: a frontend agent, a backend agent, a DevOps agent.
- They now hand work back and forth constantly.
- Each holds only a fragment of the task.

---

## Split where goals and information conflict

- Conway's Law, borrowed from org design:
  - systems mirror the org that builds them.
- Business splits legal versus sales, whose goals genuinely conflict.
- One agent owning a whole feature holds one coherent context.

---

## A good cut reduces the talking

- Better cut, fewer messages, each agent coherent.
- On paper first: many agents, then merge and re-split.
- No message left to remove means it is simple enough.

---

## Signs the cut was wrong

- Messages between two agents keep growing.
- Their briefs start contradicting each other.
- One agent mostly waits on the other.

---

## How agents pass work, most to least controllable

- Deterministic workflow: fixed steps, a code router. Easiest to debug.
- Top-down delegation: a lead spawns, reads, closes. Subagents do not talk. (Claude Code.)
- Hand-off: the agent passes the conversation to a peer, which takes over. (OpenAI Agents SDK.)
  - The transcript carries over in full or trimmed.
- Shared memory: a common file or table. Flexible, hardest to predict.
- Choreography: peers message under shared rules; a leader suggests.
- Task board: strict statuses; claim to lock; context collects on the task.

---

## The mechanism sets how debuggable you are

- A shared-memory system can pass ten tests, then break on the eleventh user.
- The trace is hard to reconstruct.
- Prefer the most controllable mechanism the task allows.

---

## Someone must merge the results

- Specialists return pieces; the orchestrator composes one answer in one call.
- Rung 2's per-class lists still need a single ordered plan.
- The merge is a designed step with its own prompt.

---

## Planner: the strong model or the weak?

- Workers often run on cheaper models; most worker calls need less model than the lead.
- But some planning and orchestration flows are trivial.

---

## A good subagent still fails on a bad brief

- The SQL subagent is capable.
- Symptoms of a bad brief: wrong data, too much data, invented data.
- This brief sits in the subagent's header, read by the caller.

---

## The brief the orchestrator reads before delegating

> IMPORTANT — NEVER pass raw SQL queries to this agent. Describe WHAT data you need and WHY (goal, context, validation criteria). The agent knows the DB schema and writes correct queries itself. Passing SQL defeats the purpose — it cannot fix your schema mistakes if you hardcode wrong table/column names.
>
> CRITICAL — This agent retrieves DATA, not DIAGNOSES. Ask for specific rows/columns/aggregates ("show all message rows for chat_id >= 572 with full content, grouped by chat_id").
>
> NEVER ask it to interpret architecture or find root causes ("find why the name changes"). The agent sees data patterns but has zero understanding of component boundaries or code flow — its interpretive narratives will be plausible but wrong. Always interpret returned data yourself against the code you've already read.

---

## What comes back is structured too

- The specialist returns a small object: `{status, result, needs_approval}`.
- The orchestrator branches on `status` instead of parsing prose.
- Session 2's structured-output discipline, applied between agents.

---

## Most multi-agent failures are interface failures

- The bug is what you pass and what you expect back.
- Write the brief for the caller.
- Treat the output as data to verify.

---

## Your job shifts toward managing and reviewing

- Several agents running, and you direct and review.
- Two stances: dictate every move, or design the environment.

---

## Durable principle: graduated responsibility transfer

- Autonomy does not start at maximum.
- An agent earns wider scope as trust, verification, and control improve.
- Operationalized by a levels-of-autonomy scale and a written delegation brief.

---

## Levels of autonomy

- A scale of how much an agent may do alone.
- Observe, suggest, act with approval, act and report.
- It moves up as it earns trust, not from the start.

---

## The delegation brief

- Write it before the system prompt.
- Scope, when it acts alone, when it asks, when it escalates.
- An effort budget: a ceiling on calls, tokens, or minutes.
- The brief pins the agent to a level on the scale.

---

## Your attention is the bottleneck

- Past a point you approve on autopilot.
- Remember the ~1:50 ratio from Session 1? Why not 1:500?

---

## Do not delegate what you cannot review

- Put the gate where an action cannot be undone.
- Surface only the few decisions that matter.

---

## A monitor that reviews the other agents

- Runs asynchronously over production runs.
- First job: find contradictions in the instructions, propose fixes.
- Side-effect: domain-knowledge sharing.

---

## In production: a daily judge job

- Samples a few records per agent type, every day.
- One random reply, judged with the full history before it.
  - a message, or tool calls plus a message.

---

## Each rubric is named values with a drawn boundary

- Prompt adherence: strictly adheres / minor violation / serious violation.
  - The boundary: a minor violation leaves the user's final outcome unchanged.
- Tone: encouraging / friendly / adversarial, and a few more.
- Grades map from the rubric values:
  - all strict: grade A
  - a minor violation: at least one grade down
  - a serious prompt violation: three grades down, whatever the style

---

## Force the judge to write a rationale

- A required field on every violation: what the judge expected, what it got.
- Without it: "serious violation" with no reason, impossible to check.
  - It looked like the judge hallucinating.
- Comparing expected against received grounds the verdict.

---

## The monitor feeds a weekly human review

- A weekly call walks the low grades; prompt violations first.
- The ops director, a manager, me and one of developers.
- Most resulting tasks change the system.
- We leave our verdicts per each reviewed case to create a dataset for tuning the LLM-as-a-judge itself.

---

## Discuss

- Where does managing agents match managing people, and where does it break?

---

<!-- _class: demo -->

## 🎬 Design clinic: a project from the room

- One volunteer describes their group's project.
- Name the one main task, and what a single agent does today.
- Together we push it toward multi-agent.
- Each agent you propose earns its place before it goes up.

---

<!-- _class: demo -->

## Does it earn a second agent?

- Name the payoff it buys:
  - parallelism
  - context economy
  - focus
  - decoupling
- Name what one agent costs without it.
- No concrete payoff means it stays one agent.

---

<!-- _class: demo -->

## Some checks before you add it

- A critic needs a checkable yardstick:
  - your tests, a rubric line, or an invariant
  - "rate it 1 to 10" is not a yardstick
- Cut on a coherent slice of the domain.
  - A job-title split just makes them message each other.
- Write its delegation brief:
  - scope, when it acts alone, asks, escalates
  - an effort budget
  - a level on the autonomy scale
- Check the whole system for a completed trifecta or inherited tools.


---

## What to carry out of today

- One agent until you can name a second one's payoff.
- Cut on conflicting goals; keep messages between agents few.
- A critic needs a real yardstick.
- Diverse, grounded agents raise reliability.
- Secure by removing tools; gate the irreversible.
- Mantra: *Delegating the work doesn't delegate the responsibility.*

---

## Homework — HW2 (course-project step)

- Due at the start of Session 6.
- Extend the course project: memory that fits its store, shared vs private, an agent pair, a monitor.
- Failure Bounty (from Session 1) is also due, as a separate submission.

---

## HW2 — three stores, and two ways to fill context

- Relational (SQLite): your domain model — films, ratings, reviews. Structured rows, not key-value.
- Non-relational (JSON / document): free-form memory the agent writes, any shape.
- Markdown: the static operating rules, edited by hand by an admin.
- Fill the context two ways:
  - push — always-on, assembled every run.
  - pull — the agent fetches when the request needs it.

---

## HW2 — what the agent remembers

- Fact: something it would re-ask; saved with a cue for when it should resurface.
  - When it surfaces, the model decides what to do with it.
- Rule: changes behavior, attached always or often; the model decides only when it applies.
- It has to decide when to save — not noise, not miss a real preference.
- It has to leave cues how to retrieve later.

---

## HW2 — shared memory and private memory

- Private: scoped to the user. A's fact never surfaces for B.
- Shared: one user writes, another user's agent reads.
  - a review on a film A writes, that B's agent sees.
  - a note on a lecture one student writes, that every student sees.
- Shared content is data, not instructions.
  - a planted "ignore your rules" comment must not steer another user's agent.

---

## HW2 — the agent pair, and the monitor

- An executor plus at least one more agent:
  - a critic on a real yardstick, or planner / verifier / merged executors.
- Structured hand-off `{status, result, needs_approval}`; a written delegation brief.
- Give them a shared memory to coordinate through — and keep it traceable.
- A monitor on its own clock over the logs — the LLM-as-a-judge from today:
  - named-value rubrics, a required rationale, reports one real problem.

---

## HW2 — submission

- The code, per-student commits as before, in the group repo.
- One trace: a fact used unprompted, a rule changing behavior, private-vs-shared, the hand-off.
- The adversarial trace: a planted comment treated as data.
- Ground your choice: why your particular multi-agent architecture was chosen.
- The monitor's report, with its rationale.
- Per member: ~1 page, including how you tested it.
