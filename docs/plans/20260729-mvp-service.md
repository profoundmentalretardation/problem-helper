# Problem Helper — MVP Service

## Overview

A Go service that helps students with programming homework **without talking to them directly**.
A frontend layer (Telegram bot, web, anything) calls our HTTP API with `user_id` + `problem_id`;
the service pulls the student's submissions from a judging platform (ejudge first, interface for
Codeforces / Yandex.Contest / LeetCode / custom), finds the defect in their best failing attempt,
verifies a fix by actually running it on the platform as a system user, and returns a **non-obvious
hint** that teaches instead of giving the answer away.

Problems it solves:
- students get pedagogical help scaled to a whole course, without a human tutor per request;
- every model call, cost and outcome is logged, so models can be A/B-tested on effectiveness vs price;
- accumulated per-student mistake profiles feed back into prompts, so help gets personal over time.

The Python notebooks (`01`–`05`) are course prototypes of the same loops; they are the *reference*,
this spec is the *priority* wherever they disagree. They move to `research/` and stay read-only.

## Context (from discovery)

- Prototypes exist for every stage: shield (`03_prompt_shield.ipynb`), repair loop (`01`),
  hint loop + cross-family checker (`02`), curator/metaloop + memory stores (`04`, `memory/`),
  monitor over a hash-chained log (`05`, `monitor.py`).
- Patterns carried over from the prototypes: gate on the irreversible action (deliver),
  checker failure ≠ approval, judge on a different model family than the writer,
  **deterministic checks before model checks** (a hopeless hint costs zero model calls),
  both-directions testing (must-catch *and* must-pass), scripted models so tests need no API key.
- The hash-chained log + monitor (`05`) has **no MVP successor** — deferred to Post-MVP;
  the `events` table is plain append-only.
- Nothing in Go exists yet; repo is notebooks + Python modules + state files.
- ⚠️ `.gitignore` currently ignores `docs/`, `state/`, `slides/` — this plan file itself is
  uncommittable until Task 1 fixes that.

## Decisions made (during planning)

| Decision | Choice |
|---|---|
| Language / shape | Go, **modular monolith** — one binary, packages split so workers could later move behind NATS JetStream, but not now |
| API style | **Async**: `POST /help` returns `request_id` immediately; caller polls `GET /requests/{id}`; work runs in goroutines |
| Databases | **Postgres only** (state + JSONB `events` for logs/analytics). Postgres is also the work queue. Schema designed so a ClickHouse move later is mechanical |
| Shield scope (MVP) | Strip comments + sanitize invalid/suspicious Unicode + record diff. Identifier anonymization (tree-sitter, v1/v2/…) is post-MVP |
| Metaloop trigger | Cron **inside the service** (nightly) + admin endpoint for manual runs |
| Mistake dedup | **LLM grouping via tools** (merge/create against the user's existing mistakes) — no embeddings; volumes are dozens per user |
| LLM access | OpenAI-compatible SDK; `LLM_BASE_URL`, `LLM_API_KEY` from env; per-agent model config (model required, temperature, reasoning effort) in YAML |
| Resume granularity | **Step-level** for MVP: a crashed request resumes at the last completed pipeline step (with platform run-id persisted so polls resume instead of re-submitting). Attempt-level checkpointing inside a loop is post-MVP |
| Naming | `problem_id` (not task_id), `user_id` (not student_id) |
| Testing | **TDD** — failing tests first, then implementation, every task |

## Development Approach

- **testing approach**: TDD — write failing tests first, implement until green, refactor
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - agent loops are tested with **scripted models** (no API key), as in the prototypes
  - platform calls are tested against a **mock platform** implementation and `httptest` fixtures
  - store tests run against **real Postgres** (dockerized); isolation via per-test transaction
    rollback (or per-test schema) — decided and written down in Task 3
  - tests assert what's on disk, not what the code claims
  - tests cover both success and error scenarios; for judges/guardrails, both directions
    (a hint that must be rejected AND one that must pass)
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` **and** `golangci-lint run` after each task — both, every time
- user (Victor) reviews every test before it is considered done

## Testing Strategy

- **unit tests**: required for every task (see above); table-driven where natural
- **integration tests**: request pipeline end-to-end with mock platform + scripted LLM,
  asserting DB state after the pipeline returns
- no UI, so no e2e browser tests; the API contract tests in Task 12 play that role

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

### Request pipeline (the whole flow, one request_id end to end)

```
POST /help {user_id, problem_id, n_submissions?}     n_submissions default: config key
  └─ create help_requests row (status=pending), return request_id      ── sync part ends here
worker claims the request (FOR UPDATE SKIP LOCKED) and runs, checkpointing after each step:
  1. platform.ProblemStatus(user, problem)
       solved?  → record, status=already_solved, stop
  2. platform.ProblemStatement(problem)  → into the request context (repair prompt needs it)
  3. platform.Submissions(user, problem, limit=n_submissions)  → snapshot into DB
       zero submissions, or none with tests_total > 0?  → status=no_submissions, stop
       (a normal outcome, not an error — the student simply hasn't submitted yet)
  4. pick best submission: max tests passed; tie → latest. Log which one was picked.
     Submission language comes from platform metadata (detection fallback);
     unsupported language → status=failed with a clear message.
  5. SHIELD: strip comments, sanitize Unicode, record diff + what was removed
  6. hint cache: hash(post-shield code); same problem + same hash has an approved hint?
       → deliver cached hint, status=done (zero LLM calls), stop.
       Cache is deliberately CROSS-USER: loop 2 sees only diff + working code, never
       user-specific data, so a hint is safe to reuse for another student with the same defect.
  7. LOOP 1 (repair): agent gets rendered system prompt (essence + placeholders: problem
     statement, cleaned user code, top-N user mistakes from DB, previous agent code or "none"),
     returns JSON {code, mistakes[]};
     optional formatter runs on the code; platform.SubmitAsSystem(code, lang) → run id is
     persisted BEFORE polling (crash-safe: resume polls the run, never re-submits);
     success = all previously-failed tests now pass AND no previously-passing test regressed;
     tools: list_test_results(first N), get_test → {input, expected, actual} for the current run;
     retries bounded by max_retries / max_cost_per_loop / max_cost_per_retry (see Cost caps);
     retries exhausted → status=no_fix;   mistakes[] → raw_mistakes table
  8. LOOP 2 (hint): agent gets diff(cleaned user code, working code) + working code,
     returns JSON {hint};
     deterministic pre-checks FIRST (quoted repaired code, line numbers, "change X to Y",
     call expressions lifted from changed lines — corpus ported from 02_hint_loop) — a hint
     failing these never reaches the guardrail model;
     GUARDRAIL (heavier model, different family) gets diff + code + hint, returns JSON
     {approved, reason}; approved must be explicit — prose, wrong schema, dead connection are
     NOT approval; rejection reason feeds the next attempt; the same hint proposed twice → stop;
     retries exhausted → status=no_hint
  9. deliver: store hint, delivery recorded as an event, status=done; caller sees it on poll
every step → events row (request_id, kind, payload) + llm_calls row per model call
```

### Nightly metaloop (curator)

Cron inside the service walks completed-but-unprocessed requests, collects `raw_mistakes`,
and an LLM agent with tools (`merge_into(mistake_id)`, `create_mistake`, `finish`) folds them
into per-user `mistakes` (typed, dated, counted). Top-N (ordered by `count` desc, then
`last_seen` desc) go into the repair agent's system prompt. Manual trigger via admin endpoint.

### Key design decisions and rationale

- **Gate on delivery, verify by execution.** A hint is only released after the fixed code
  actually passed on the platform and a separate guardrail approved the wording. Writing is
  free; delivering is irreversible (pattern from `02_hint_loop`).
- **Deterministic before model.** Both the shield and the hint gate run cheap deterministic
  rules before any model call — hopeless cases cost zero tokens.
- **Diff into loop 2, not two full files** — keeps context small and points both models at
  exactly what changed; working code rides along for grounding.
- **Everything is a row.** Analytics, A/B tests, cost accounting, resume-after-crash and the
  admin `useless` flag all fall out of the same `events`/`llm_calls`/`help_requests` tables.
- **Postgres is the queue.** Claim = `SELECT … FOR UPDATE SKIP LOCKED`; workers heartbeat;
  a `running` row with a stale heartbeat is reclaimed and resumed at its last completed step.

## Technical Details

### Statuses and transitions

```
pending → running → { already_solved, no_submissions, done, no_fix, no_hint, failed }
running → pending          (reclaim of a crashed worker's row)
```
Terminal statuses and their meaning in `GET /requests/{id}`:
- `already_solved` — message "problem already solved, nothing to do"
- `no_submissions` — message "no submissions to analyze yet"
- `done` — the hint text (from cache or fresh)
- `no_fix` — repair loop exhausted retries/cost; no working code found
- `no_hint` — working code found but no hint passed the guardrail
- `failed` — infrastructure/platform error, `error` field populated

`no_fix`/`no_hint` are distinct from `failed` on purpose: analytics must separate
"our infra broke" from "we declined to give a hint".

### Configuration

- Env (required): `DATABASE_URL`, `LLM_BASE_URL`, `LLM_API_KEY`, `PLATFORM` (=`ejudge`),
  `API_TOKEN` (bearer for `/help` + `/requests`), `ADMIN_TOKEN` (bearer for `/admin/*`),
  platform credentials (`EJUDGE_URL`, `EJUDGE_SYSTEM_LOGIN`, `EJUDGE_SYSTEM_PASSWORD`, …)
- `agents.yaml` (checked in; **all four agent keys required**, unknown keys rejected):
  ```yaml
  defaults:
    n_submissions: 25        # POST /help default when n_submissions omitted
    daily_requests_per_user: 20
  repair:
    model: "..."             # required, no default — same fields for every agent below
    temperature: 0.2         # optional
    reasoning_effort: ""     # optional
    max_retries: 3
    max_cost_per_retry: 0    # 0 = unlimited; see Cost caps for enforcement points
    max_cost_per_loop: 0
    top_n_mistakes: 5        # how many user mistakes into the prompt
    n_tests_shown: 10        # list_test_results cut-off
  hint:      { model: "...", temperature: 0.7, max_retries: 3, max_cost_per_retry: 0, max_cost_per_loop: 0 }
  guardrail: { model: "..." }                # heavier model, different family recommended
  curator:   { model: "...", max_retries: 2 }
  pricing:                   # per 1M tokens, per model; startup FAILS if any configured
    "model-name": {input: 0, cached_input: 0, output: 0}   # agent model has no pricing entry
  formatter:
    enabled: false
    command: ""              # e.g. "gofmt", "clang-format -style=file", per course rules
  ```
- The checked-in `agents.yaml` itself must parse and validate in a test.
- Prompts live in `prompts/*.md` with `{{placeholders}}`, loaded/validated at startup by
  `internal/prompt`; a missing placeholder at render time is an error, not a silent blank.
  "No previous code" / "no recorded mistakes" render as explicit "none" text.

### Cost caps (enforcement points, exactly)

Cost of a call is known only after it returns, so:
- **`max_cost_per_loop`**: checked **before starting each attempt** — if the loop's accumulated
  cost ≥ cap, stop with reason `cost_cap` (→ `no_fix`/`no_hint`).
- **`max_cost_per_retry`**: checked **between tool-loop calls within one attempt** — if the
  attempt's accumulated cost ≥ cap, abort the attempt (counts as a used retry).
- `0` disables the check. Both caps can therefore overshoot by at most one call — documented,
  accepted for MVP.
- Cost formula (cached tokens are a **subset** of input tokens in OpenAI-compatible usage):
  `cost = (input_tokens − cached_tokens)·p_in + cached_tokens·p_cached + output_tokens·p_out`.
  Stored as `numeric`, never float.

### Postgres schema (tables, main columns)

- `help_requests` — id (uuid), user_id, problem_id, platform, n_submissions_taken,
  status (see transitions), failure_reason, best_submission_id, hint_id,
  useless (bool, default false, admin-set), error, claimed_by, heartbeat_at,
  resume_step, created_at, updated_at
- `submissions` — snapshot per request: platform_submission_id, code, **language**,
  tests_passed, tests_total, submitted_at, is_best
- `shield_records` — request_id, code_before, code_after, diff, removed (jsonb: comments,
  unicode, counts) — all three stored deliberately, audit-friendly
- `llm_calls` — request_id, agent, model, input_tokens, cached_input_tokens, output_tokens,
  cost (numeric), latency_ms, attempt, created_at (+ prompt/response payload for audit)
- `hints` — id, request_id (originating), problem_id, code_hash (post-shield best code),
  text, approved, created_at. Delivery (incl. cache re-delivery to another request) is recorded
  via `events` + `help_requests.hint_id`, NOT a flag here — one hint row can serve many requests
- `raw_mistakes` — request_id, user_id, text, created_at, processed (bool)
- `mistakes` — **id**, user_id, title, description, count, first_seen, last_seen
- `events` — request_id, kind, payload (jsonb), created_at — append-only, the analytics feed

Indexes the design depends on: `hints (problem_id, code_hash) WHERE approved`,
`raw_mistakes (user_id) WHERE NOT processed`, `events (request_id, created_at)`,
`help_requests (status)`, `mistakes (user_id)`.

### Go layout

```
cmd/helper/            single binary: HTTP server + worker goroutines + cron
internal/config/       env + agents.yaml
internal/store/        pgx + migrations + typed queries
internal/platform/     Platform interface; platform/ejudge/; platform/mock/
internal/pick/         best-submission selection
internal/shield/       comment stripping, unicode sanitizing, diff
internal/llm/          OpenAI-compatible client, usage & cost accounting
internal/prompt/       template loading + placeholder rendering
internal/agent/        repair/, hint/ (incl. guardrail), curator/
internal/format/       optional external formatter runner
internal/worker/       pipeline orchestration, queue claim, resume, cron
internal/api/          HTTP handlers (help, requests, admin) + auth + rate limit
migrations/            SQL migrations
prompts/               system prompts with placeholders
research/              frozen Python prototypes (notebooks, memory/, state/, …)
```

### Platform interface (first cut)

```go
type Platform interface {
    ProblemStatement(ctx, problemID) (Statement, error)
    ProblemStatus(ctx, userID, problemID) (Status, error)         // solved / unsolved
    Submissions(ctx, userID, problemID, limit int) ([]Submission, error)  // incl. Language
    SubmitAsSystem(ctx, problemID, code, lang string) (RunResult, error)  // RunResult.ID persisted before polling
    RunResult(ctx, runID string) (RunResult, error)               // poll an existing run
    TestResult(ctx, runID string, testID int) (TestCase, error)   // input, expected, actual — per RUN, not per problem
}
```
⚠️ Hidden test inputs are the norm on judges; if ejudge exposes only verdicts for most tests,
`TestResult` degrades to `{index, verdict}` and the repair prompt is written around that.
Verified against the real platform in Task 15 (deliberately late — everything before it runs
against `platform/mock`).

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): everything achievable in this repo — restructure,
  docs, Go code, migrations, tests
- **Post-Completion** (no checkboxes): real ejudge credentials & live trial, frontend/Telegram
  integration, deployment, and the post-MVP ideas list

## Implementation Steps

### Task 1: Restructure repo, write AGENTS.md, symlink CLAUDE.md

**Files:**
- Create: `AGENTS.md`, `CLAUDE.md` (symlink → AGENTS.md), `research/README.md`
- Modify: `.gitignore`
- Move: `0*.ipynb`, `idea.ipynb`, `monitor.py`, `ask_tutor.py`, `memory/`, `rules/`,
  `scratch/`, `state/`, `traces/`, `slides/`, `requirements.txt` → `research/`

- [x] fix `.gitignore` FIRST: remove `docs/`, `state/`, `slides/` lines (they hide this plan);
      add `research/state/*.db`; keep `.venv/`, `.env`, `__pycache__/`, `.ipynb_checkpoints/`
- [x] move prototypes into `research/`: `git mv` for tracked paths (notebooks, `memory/`,
      `rules/`, `scratch/`, `traces/`, `monitor.py`, `ask_tutor.py`, `requirements.txt`);
      plain `mv` + `git add` for previously-ignored `state/`, `slides/`; drop `__pycache__`
- [x] write `AGENTS.md`: project description (Overview + pipeline diagram), Go layout,
      conventions (TDD, scripted-model tests, `go test ./...` + lint before done, guardrail on
      a different model family), pointer to `research/` as reference-only
- [x] `ln -s AGENTS.md CLAUDE.md`, commit the symlink
- [x] write `research/README.md`: what each notebook prototyped and which internal package
      supersedes it; note explicitly that `05_monitor` (hash-chained log + monitor) has no MVP
      successor and is listed in Post-MVP ideas
- [x] verify: `git status` clean after commit, `docs/plans/` tracked,
      `git log --follow research/monitor.py` shows pre-move history

### Task 2: Go module skeleton and config loading

**Files:**
- Create: `go.mod`, `cmd/helper/main.go`, `internal/config/config.go`, `internal/config/config_test.go`, `agents.yaml`, `Makefile`, `.golangci.yml`

- [x] write failing tests: env parsing (each required var missing → named error), `agents.yaml`
      parsing (all four agent keys required, model required per agent, unknown agent key →
      error, 0 = unlimited caps, negative caps → error, defaults block, formatter block)
- [x] write failing test: startup fails if any configured agent model lacks a `pricing` entry
- [x] write failing test: the checked-in `agents.yaml` itself parses and validates
- [x] `go mod init`, minimal `main.go` (config load + log + exit), Makefile targets:
      `build`, `test`, `lint`, `run`; implement `internal/config` until green
- [x] run tests + lint - must pass before task 3

### Task 3: Postgres store, migrations, event log

**Files:**
- Create: `migrations/0001_init.sql`, `internal/store/store.go`, `internal/store/store_test.go`, `docker-compose.yml` (postgres for dev/test)

- [x] decide and document test isolation (per-test transaction rollback vs per-test schema)
      in a comment at the top of `store_test.go`
- [x] write failing tests (real dockerized Postgres): create help_request, legal status
      transitions succeed, **illegal transition → error** (graph from Technical Details),
      append event, insert llm_call with `numeric` cost, snapshot submissions with language —
      assert rows on disk
- [x] write migrations for all tables + indexes from Technical Details; migration runner wired
      into service start
- [x] implement `internal/store` (pgx) until green
- [x] write error-case tests: duplicate request insert, event for unknown request
- [x] run tests + lint - must pass before task 4

### Task 4: Platform interface, mock platform, best-submission picker

**Files:**
- Create: `internal/platform/platform.go`, `internal/platform/mock/mock.go`, `internal/pick/pick.go`, `internal/pick/pick_test.go`, `internal/platform/mock/mock_test.go`

- [x] write failing tests for picker: max tests passed wins; tie → latest submission;
      empty list → typed `ErrNoSubmissions`; all submissions `tests_total == 0` (compile
      errors only) → also `ErrNoSubmissions`
- [x] define `Platform` interface + domain types exactly as in Technical Details
      (Submission **with Language**, RunResult **with ID**, TestCase, Status)
- [x] implement mock platform (scriptable: canned submissions, canned run results, canned
      per-run test cases; raises on unscripted calls) — the test double for every later task
- [x] implement picker until green
- [x] run tests + lint - must pass before task 5

### Task 5: Shield (sanitize + diff record)

**Files:**
- Create: `internal/shield/shield.go`, `internal/shield/shield_test.go`

- [x] write failing tests: strips `//`, `/* */`, `#` (Python) and docstring comments,
      **dispatched by the submission's language** — `#define`/preprocessor directives preserved
      in C/C++; removes invalid & confusable Unicode (NFC normalize, strip zero-width/bidi
      controls); returns diff + structured removed-items report
- [x] write must-pass tests: clean code passes byte-identical; string literals containing
      comment-like text are NOT stripped
- [x] port the injection corpus from `research/03_prompt_shield.ipynb` as table-driven
      must-catch cases (payload in comments must not survive)
- [x] implement shield until green; wire output to `shield_records` (real store, per Task 3
      isolation)
- [x] run tests + lint - must pass before task 6

### Task 6: LLM client with usage and cost accounting

**Files:**
- Create: `internal/llm/client.go`, `internal/llm/cost.go`, `internal/llm/client_test.go`, `internal/llm/scripted.go` (scripted model for tests)

- [x] write failing tests: chat call with JSON-schema response (parse + validate + one retry
      on invalid JSON), usage extraction (input / cached-subset-of-input / output)
- [x] write failing table-driven tests for the cost formula from Technical Details, including
      the cached-tokens-are-a-subset case (the double-counting trap)
- [x] implement OpenAI-compatible client (base URL + key from config; per-agent
      model/temperature/effort); every call writes an `llm_calls` row (agent, model, tokens,
      cost as numeric, attempt)
- [x] implement `scripted.go`: deterministic fake fulfilling the same interface — replays
      canned responses, fails the test if consulted when it shouldn't be
- [x] run tests + lint - must pass before task 7

### Task 7: Prompt loading and rendering

**Files:**
- Create: `internal/prompt/prompt.go`, `internal/prompt/prompt_test.go`, `prompts/repair.md`, `prompts/hint.md`, `prompts/guardrail.md`, `prompts/curator.md`

- [x] write failing tests: `{{placeholder}}` rendering; missing placeholder value at render
      time → error (never a silent blank); empty previous-code / empty mistakes render as
      explicit "none" text; startup load validates every file in `prompts/`
- [x] implement `internal/prompt` until green
- [x] write first real drafts of the four prompts (essence of each agent, placeholders wired)
- [x] run tests + lint - must pass before task 8

### Task 8: Repair loop (loop 1)

**Files:**
- Create: `internal/agent/repair/repair.go`, `internal/agent/repair/repair_test.go`

- [x] write failing tests (scripted LLM + mock platform): happy path — agent returns
      {code, mistakes}, platform run passes, loop ends with working code and raw_mistakes rows
- [x] write failing test for the success rule: previously-failed tests pass **but a
      previously-passing test regresses → NOT success**, retry continues
- [x] write failing tests for bounds per the Cost caps section: max_retries exhausted →
      `no_fix`; per-loop cap checked before an attempt; per-retry cap checked between
      tool-loop calls; 0 = unlimited
- [x] write failing tests for tools: `list_test_results` truncated to n_tests_shown,
      `get_test` returns the current run's {input, expected, actual}, run/problem ids injected
      programmatically (model never chooses them)
- [x] implement: prompt assembly via `internal/prompt` (statement, cleaned code, top-N
      mistakes, previous agent code), JSON schema `{code, mistakes[]}`, tool loop,
      success rule, run-id persisted before polling
- [x] run tests + lint - must pass before task 9

  ⚠️ `llm.ChatClient` has no native tool-calling channel (schema-validated JSON only), so the
  repair "tools" (`list_test_results`, `get_test`) are emulated with one response schema
  carrying a discriminated `action` field (`list_test_results` | `get_test` | `submit`)
  instead of the OpenAI-style `tools`/`tool_calls` wire format the Python prototype used.
  `EventRecorder`/`MistakeRecorder` are narrow interfaces satisfied by `*store.Store`
  (`AppendEvent`, and a new `InsertRawMistake`/`ListRawMistakes` pair added to
  `internal/store`); repair-loop tests use in-memory fakes, store tests use real Postgres,
  per the plan's testing conventions.

### Task 9: Optional formatter step

**Files:**
- Create: `internal/format/format.go`, `internal/format/format_test.go`
- Modify: `internal/agent/repair/repair.go`

Rationale: the course has formatting rules; agent code should be formatted before it is
submitted and before it is diffed for loop 2 (readable diffs), so the step sits between
repair-agent output and everything downstream.

- [x] write failing tests: disabled → code untouched; enabled → external command run on code,
      output used; command fails or times out → original code used + warning event (the
      formatter must never kill the loop)
- [x] implement runner (exec with timeout), wire into repair loop after agent output
- [x] run tests + lint - must pass before task 10

### Task 10: Hint loop + guardrail (loop 2)

**Files:**
- Create: `internal/agent/hint/hint.go`, `internal/agent/hint/rules.go`, `internal/agent/hint/guardrail.go`, `internal/agent/hint/hint_test.go`

- [x] write failing table-driven tests for **deterministic pre-checks** (ported from
      `research/02_hint_loop`): quoted repaired code, line numbers, "change X to Y", call
      expressions lifted from changed lines — four must-catch AND two must-pass cases
      (a checker rejecting everything is useless)
- [x] write failing tests (scripted models): hint agent gets diff + working code → {hint};
      guardrail gets diff + code + hint → {approved, reason}; rejection reason fed to next
      attempt; **same hint proposed twice → stop, don't burn retries**
- [x] write both-directions gate tests: explicit approved=true → hint stored approved;
      approved=false → retry; **prose / wrong schema / connection error → NOT approved**
- [x] write bounds tests: max_retries / cost caps per the Cost caps section; exhausted → `no_hint`
- [x] implement diff computation, pre-checks, both agents (guardrail on its own `agents.yaml`
      entry), until green
- [x] run tests + lint - must pass before task 11

### Task 11: Hint cache lookup

**Files:**
- Create: `internal/worker/cache.go`, `internal/worker/cache_test.go`

Scope: pure lookup — `Lookup(problemID, codeHash) (*Hint, bool)` over the `hints` table.
The "cache hit skips the LLM entirely" end-to-end assertion lives in Task 13.

- [x] write failing tests (real store): hit on (problem_id, post-shield hash, approved=true);
      miss on different hash / unapproved hint / different problem; hash is of normalized
      post-shield code
- [x] implement lookup + hash until green
- [x] run tests + lint - must pass before task 12

### Task 12: HTTP API handlers, auth, rate limit

**Files:**
- Create: `internal/api/api.go`, `internal/api/api_test.go`

- [x] write failing tests (handlers against a fake worker/store): `POST /help
      {user_id, problem_id, n_submissions?}` → 202 + request_id, default n_submissions from
      config; `GET /requests/{id}` → per-status payloads exactly as in Technical Details
      (already_solved / no_submissions / done+hint / no_fix / no_hint / failed+error);
      validation → 400; unknown id → 404
- [x] write failing auth tests: missing/wrong bearer on `/help`+`/requests` → 401
      (`API_TOKEN`); on `/admin/*` → 401 (`ADMIN_TOKEN`)
- [x] write failing rate-limit test: user over `daily_requests_per_user` → 429
- [x] implement handlers + middleware until green
- [x] run tests + lint - must pass before task 13

### Task 13: Pipeline orchestration

**Files:**
- Create: `internal/worker/pipeline.go`, `internal/worker/pipeline_test.go`

- [x] write failing tests (mock platform + scripted LLM + real store), one per path:
      solved-early-exit; no_submissions; **cache hit → hint delivered with ZERO model calls**
      (scripted model raises if consulted) and a `hint_cache_hit` event; full path through
      shield → repair → format → hint → guardrail → delivered
- [x] write failing tests: every step leaves its `events` row; `n_submissions_taken` recorded;
      unsupported language → `failed` with clear message; `no_fix` and `no_hint` terminal paths
- [x] implement the pipeline (steps 1–9 of the diagram) as a function of (request, deps),
      checkpointing `resume_step` after each step
- [x] run tests + lint - must pass before task 14

  ⚠️ Task 3's store had no setters for `resume_step`, `best_submission_id`, `hint_id`,
  `failure_reason`, `error` (only `TransitionStatus`/`AppendEvent`/inserts existed) — added
  `SetResumeStep`, `SetBestSubmission`, `SetHintID`, `SetFailureReason`, `SetError` to
  `internal/store/store.go` with their own tests in `store_test.go`, since the pipeline needs
  them to record outcomes. The formatter step (Task 9) already lives inside
  `repair.Runner.Run`, so the pipeline calls the repair loop once and gets formatted code back;
  it does not call `internal/format` separately. Branching on an existing `resume_step` to skip
  completed work on a crash-reclaimed row is left to Task 14, which owns claim/reclaim
  mechanics — Task 13 only writes the checkpoints.

### Task 14: Queue claim, crash resume, wiring

**Files:**
- Create: `internal/worker/worker.go`, `internal/worker/worker_test.go`
- Modify: `cmd/helper/main.go`

- [x] write failing concurrency test: two workers, one pending row → exactly one claims it
      (`FOR UPDATE SKIP LOCKED`)
- [x] write failing reclaim test: row in `running` with stale `heartbeat_at` → reclaimed,
      resumed at `resume_step` (not restarted from step 1); pending platform run polled by
      persisted run-id, **never re-submitted**
- [x] write failing shutdown test: SIGTERM → in-flight request finishes its current step,
      heartbeat stops, process exits
- [x] implement worker pool (claim loop, heartbeat, reclaim-on-startup + periodic), graceful
      shutdown; wire config + store + api + worker in `main.go`
- [x] manual test (partially automatable): `docker compose up` (no `docker-compose.yml`
      orchestration beyond Postgres exists yet) + running the binary confirmed HTTP serves,
      `POST /help` enqueues, the worker claims and runs the pipeline, SIGTERM drains cleanly.
      "poll → hint delivered" isn't reachable by hand yet — `platform/mock` (main.go's stand-in
      until Task 15) starts empty and panics on any unscripted call, and there's no admin
      endpoint to seed it; skipped rather than faked. A panic mid-pipeline was hit doing this
      (`mock: unscripted ProblemStatus`) and exposed a real gap — an unrecovered panic in a
      claimed run crashed the whole process — now fixed with a `recover()` boundary in
      `Worker.RunOnce` (a bad claim fails and is left for reclaim, not fatal to the pool).
- [x] run tests + lint - must pass before task 15

### Task 15: ejudge platform implementation

**Files:**
- Create: `internal/platform/ejudge/ejudge.go`, `internal/platform/ejudge/ejudge_test.go`, `internal/platform/ejudge/testdata/*`

Deliberately after the mock-based pipeline: the MVP is demonstrable end-to-end regardless of
what ejudge integration uncovers.

- [x] FIRST: capture real request/response samples from an ejudge instance (or local docker
      ejudge) into `testdata/`; record whether the surface is JSON or HTML/CGI and which of
      statement / status / submissions / submit / run-result / per-test data are available
- [x] write failing tests against `httptest` replaying the captured fixtures: statement,
      status, submissions (with language), submit as system user, poll run by id, per-test
      results (or the degraded `{index, verdict}` form — update the repair prompt if so)
- [x] implement ejudge client until green (auth, retries on transient errors)
- [x] write error-case tests: auth failure, timeout, malformed response, run stuck in queue →
      typed errors surfacing as request `failed`
- [x] run tests + lint - must pass before task 16

**Findings against a live ejudge 3.8.0 docker instance** (see `internal/platform/ejudge/ejudge.go`
package doc for full detail):
- Surface is classic HTML/CGI, not JSON: the `*_JSON` actions in ejudge's own protocol header
  (`NEW_SRV_ACTION_LOGIN_JSON` etc.) exist but returned malformed/unusable output against this
  build; the client scrapes the same HTML pages a browser would render, using two sessions —
  `new-client` (participant role) for statements and submitting, `new-master` (Administrator
  role) for another user's status/submissions/run detail.
- Per-test detail (input/expected/actual) is fully available to the Administrator role on this
  instance, so `TestResult` is NOT degraded to `{index, verdict}` here — the plan's anticipated
  fallback path is still implemented (see `TestResult` doc comment) in case a course configures
  a contest that hides it even from admins.
- One real degradation found: this course's `serve.cfg` uses `score_system = acm`, which halts
  judging at the first failing test. For a non-passing run, `TestsTotal` therefore reflects
  tests actually judged before ejudge stopped, not the problem's true hidden test count — no
  ejudge endpoint (including Administrator-only ones) reports the true total independently of
  a run's outcome. Documented in the package doc; does not affect `pick.Best`, which only
  compares `TestsPassed` within one problem's submissions.
- This sandbox's ejudge instance has exactly one registered user (the built-in `ejudge`
  administrator, who is also registered as a contest participant), so captured fixtures
  necessarily use that single login for both the "system user" and "student" roles. The
  query surface itself (`new-master`'s `filter_expr`, scoped by `login`/`prob`) is
  user-parameterized and works identically for any login — nothing in the client is hardwired
  to that one account beyond the fixtures.

### Task 16: Metaloop curator (raw_mistakes → mistakes) + cron

**Files:**
- Create: `internal/agent/curator/curator.go`, `internal/agent/curator/curator_test.go`
- Modify: `internal/worker/worker.go` (cron), `internal/api/api.go` (admin trigger)

- [x] write failing tests (scripted LLM): unprocessed raw_mistakes + the user's existing
      mistakes go to the agent; `merge_into(mistake_id)` increments count and updates
      last_seen; `create_mistake` inserts; `finish` marks raw rows processed
- [x] write both-directions tests: near-duplicate must merge, genuinely new must create;
      agent returning garbage → nothing written, rows stay unprocessed (retried next night)
- [x] write top-N query test: ordered by count desc then last_seen desc, limit
      `top_n_mistakes` — this is what Task 8's prompt consumes
- [x] implement curator with the three tools, nightly in-service cron,
      `POST /admin/metaloop/run` manual trigger (ADMIN_TOKEN)
- [x] run tests + lint - must pass before task 17

### Task 17: Analytics queries + admin endpoints

**Files:**
- Create: `internal/store/analytics.go`, `internal/store/analytics_test.go`
- Modify: `internal/api/api.go`

- [x] write failing tests: cost per request / per model / per agent from llm_calls; request
      counts by status (no_fix vs no_hint vs failed separable); hint-effectiveness inputs
      (submission snapshot counts + timestamps, so "how many submissions until solved after
      the hint" is computable downstream)
- [x] write failing admin tests: `POST /admin/requests/{id}/useless` sets the flag; request
      listing filterable by useless / status / model
- [x] implement queries and endpoints until green
- [x] run tests + lint - must pass before task 18

### Task 18: Verify acceptance criteria

- [x] walk the pipeline diagram step by step against the code — every step, status and event
      present. Found and fixed real gaps: `cmd/helper/main.go` was hardcoding `platform/mock`
      even after Task 15 shipped the ejudge client, ignoring the required `PLATFORM`/`EJUDGE_*`
      config entirely — added `newPlatform` to select ejudge vs mock from `cfg.Env.Platform`.
      Steps 2 (`ProblemStatement`) and 5 (shield) wrote no `events` row — added
      `problem_statement` and `shield_applied` events. Steps 7/8 (repair/hint loops): a platform
      error surfacing from inside either loop was returned as a bare Go error instead of
      transitioning the row to `failed`, contradicting `RunPipeline`'s own doc comment — fixed
      by routing those errors through `infraFail`.
- [x] verify edge cases: solved-early-exit, no_submissions, cache hit, regression-blocking
      success rule, guardrail never-approves → `no_hint`, platform down mid-loop → `failed`,
      crash-reclaim-resume without re-submitting. All implemented and tested; added
      `platform/mock` error-scripting (`ScriptStatusError`, `ScriptSubmitError`) plus
      `TestRunPipeline_PlatformErrorFetchingStatus_Failed` and
      `TestRunPipeline_PlatformErrorDuringRepairLoop_Failed` to close the previously-untested
      "platform down mid-loop" edge case (this is what exposed the steps 7/8 bug above). The
      crash-reclaim-resume case never re-submits at the pipeline-step granularity (tested,
      `TestRunPipeline_ResumesAtCheckpoint_SkipsCompletedSteps` /
      `TestWorker_ReclaimedRequest_IsReClaimedAndRun`); a crash strictly inside an in-flight
      repair-loop attempt re-enters that attempt from scratch on resume — documented as
      intentional post-MVP scope in `pipeline.go`'s header comment, per the plan's "Resume
      granularity" decision (attempt-level checkpointing inside a loop is post-MVP).
- [x] run full test suite: `go test ./...` — all packages pass
- [x] run `golangci-lint run` — clean, 0 issues
- [x] check test coverage: `go test -cover ./...`, no package silently at 0% (only `cmd/helper`,
      the main-wiring entrypoint, and `internal/platform`, a pure interface file, have no test
      files — both expected)
- [x] full smoke run via docker-compose with mock platform: ran the real binary against the
      dockerized Postgres with `PLATFORM=mock` — HTTP serves, `POST /help` (with bearer token)
      enqueues and returns `request_id` (202), the worker claims and runs it, `GET /requests/{id}`
      reflects `running`, SIGTERM drains cleanly, events/help_requests rows inspected by hand via
      `psql`. Reaching `hint delivered` end-to-end needs mock platform data seeded before the
      worker's first (panic-on-unscripted-call) platform call, and there is still no admin seed
      endpoint for that — same limitation already recorded in Task 14's manual-test note;
      skipped rather than faked. The full happy-path-to-`done` pipeline (incl. hint delivery) is
      already covered end-to-end by `TestRunPipeline_FullHappyPath` against the same dockerized
      Postgres, scripted LLM, and mock platform.

### Task 19: [Final] Update documentation

- [x] update `AGENTS.md` with patterns/conventions discovered during implementation
- [x] write `README.md` (what it is, how to run, config reference incl. auth tokens)
- [ ] move this plan to `docs/plans/completed/` — un-archived for the post-implementation
      code review (Task 20); re-archive once that review is closed out

### ➕ Task 20: Post-review hardening

Found during code review of the completed implementation. Every item ships with tests.

- [x] **atomic status transition** — `store.TransitionStatus` was a `SELECT ... FOR UPDATE`
      followed by a separate `UPDATE`; on a pool-bound `Store` each runs in its own implicit
      transaction, so the lock was released between them and two workers racing over a reclaimed
      row could both win. Rewritten as one CTE statement; `ErrIllegalTransition.From` now comes
      from the pre-`UPDATE` snapshot.
- [x] **`POST /help` input limits** — 8 KiB body, 128-char `user_id`/`problem_id`, `n_submissions`
      in `1..200`, all returning `400`. Boundary-accept cases are tested too.
- [x] **ejudge `filter_expr` injection guard** — `user_id` reaches us untrusted and is
      interpolated into an expression ejudge parses server-side, where Go's `%q` is not ejudge's
      escaping. `loginRe` rejects anything outside `[A-Za-z0-9._@-]{1,64}` before any request
      goes out. Tested both directions.
- [x] **`MergeMistake` is user-scoped** — the `mistake_id` comes from a model that can
      hallucinate a well-formed uuid; without the `user_id` predicate a sweep for one student
      could bump another's tally. A miss is fed back to the model as a tool error rather than
      failing the sweep, which would strand the batch forever.
- [x] **guardrail-independence enforced at startup** — `config.ParseAgents` rejects a guardrail
      whose model family matches the hint writer's. Families compare on the id's *last* path
      segment, so `openai/gpt-4.1-mini` vs `gpt-4.1-mini` is caught as the same model written
      two ways, while two models behind one gateway prefix are not falsely collapsed.
- [x] **repair verification requires the judge's accept verdict** — `success` compared only the
      baseline run's tests. Under `acm` scoring the judge halts at the first failure, so a
      student's failing run has results only up to that point: code passing exactly those and
      failing a later test cleared the check and was delivered as a "verified" fix.
      `RunResult.Passed` is now required as well.
- [x] **verification polling is bounded and cancellable** — wall-clock `MaxPollWait` (default
      5 min) on a derived context, so it bounds the platform call inside each poll and not just
      the sleeps between them; a wedged judge run can no longer pin a worker slot. Also added
      `ReasonNoBaseline`: an empty baseline can never satisfy `success`, so it short-circuits
      before any model call or verification submission.
- [x] **curator call budget sized from the batch** — was `max_retries` alone, which made any
      batch needing more actions than that permanently unfinishable. Now one call per raw
      mistake plus `max_retries` of slack.
- [x] **shield comment-stripper apostrophe handling** — C++14 digit separators (`1'000'000`) were
      read as char-literal quotes, and the fix for that then swallowed encoding prefixes
      (`L'"'`, `u8'"'`), opening a phantom string that hid the rest of the line from the shield.
      A separator is now recognized only between two hex digits. Unterminated literals no longer
      cross a newline.
- [x] **`shield.Canonical` language aliases** — platforms name languages after the compiler
      (`g++`, `python3`, `java8`), so `Submission.Language` almost never equalled a `Lang*`
      constant and real submissions routed to "unsupported language".
- [x] **formatter empty-output guard** — a formatter exiting 0 having printed nothing would
      submit an empty file for verification; caller cancellation is now reported distinctly from
      a timeout.
- [x] **`hint_delivered` on the cache-hit path** — `HintEffectivenessInputs` keys off that event,
      so cached re-deliveries were invisible to the analytics.
- [x] **`top_n_mistakes` wired through** — `Pipeline.TopNMistakes` is set from `agents.yaml` in
      `cmd/helper/main.go` and rendered into the repair prompt. This is the read side of the
      curator loop; without it the nightly sweep built a profile nothing consumed.
- [x] **failure detail redacted from callers, kept for operators** — `GET /requests/{id}` returns
      a fixed message for `status=failed` (`help_requests.error` can carry provider response
      bodies, ejudge URLs, DB text); the full string is exposed on `GET /admin/requests`, behind
      `ADMIN_TOKEN`.
- [x] **cross-package test isolation for queue tests** — `internal/worker`'s claim-race test
      commits a pending row, which `internal/store`'s `ClaimNext`/`Heartbeat`/`ReclaimStale`
      tests could see (they range over the whole table, and `go test ./...` runs package
      binaries concurrently against one database). Both sides now take a Postgres advisory
      lock; the suite was run repeatedly to confirm the flake is gone.
- [x] `go test ./...` and `golangci-lint run` clean

## Post-Completion

*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- live trial against a real ejudge instance with system-user credentials (env-configured)
- prompt quality pass with a real model: are hints actually implicit? tune `prompts/*.md`
- guardrail calibration: seed known-bad hints (line numbers, "change X to Y") and confirm
  rejection rate
- decide retention policy for `llm_calls` prompt/response payloads (they grow fast)

**External system updates:**
- frontend/Telegram-bot layer calls `POST /help` and polls — out of this repo's scope
- deployment (systemd unit / container), secrets management for platform + LLM keys
- Russian-language injection patterns for shield rules (known gap inherited from prototypes)

**Post-MVP ideas (explicitly deferred):**
- identifier anonymization in shield via tree-sitter (v1/v2/… renaming)
- attempt-level resume checkpoints inside loops (MVP resumes at step granularity)
- hash-chained event log + monitor agent (successor of `05_monitor` / `monitor.py`)
- events offload to ClickHouse if volume demands it
- pgvector embeddings for mistake matching at scale
- worker extraction behind NATS JetStream if load requires separate processes
- A/B harness over `llm_calls` (model column is already there) for cost-vs-quality experiments
