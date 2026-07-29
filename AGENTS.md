# Problem Helper — MVP Service

A Go service that helps students with programming homework **without talking to them directly**.
A frontend layer (Telegram bot, web, anything) calls our HTTP API with `user_id` + `problem_id`;
the service pulls the student's submissions from a judging platform (ejudge first, interface for
Codeforces / Yandex.Contest / LeetCode / custom), finds the defect in their best failing attempt,
verifies a fix by actually running it on the platform as a system user, and returns a
non-obvious hint that teaches instead of giving the answer away.

Problems it solves:
- students get pedagogical help scaled to a whole course, without a human tutor per request;
- every model call, cost and outcome is logged, so models can be A/B-tested on effectiveness vs price;
- accumulated per-student mistake profiles feed back into prompts, so help gets personal over time.

The full spec, decisions, schema, and task breakdown live in
`docs/plans/20260729-mvp-service.md` — this file is the orientation layer, that plan
is the source of truth. See `README.md` for how to run the service and the API/config reference.

## Pipeline (one request_id end to end)

```
POST /help {user_id, problem_id, n_submissions?}     ── sync part ends here, returns request_id
worker claims the request and runs, checkpointing after each step:
  1. platform.ProblemStatus       → solved? stop (already_solved)
  2. platform.ProblemStatement    → into request context
  3. platform.Submissions         → snapshot; none usable? stop (no_submissions)
  4. pick best submission (max tests passed, tie → latest)
  5. SHIELD: strip comments, sanitize Unicode, record diff
  6. hint cache: hash(post-shield code) already has an approved hint? → deliver, stop (done)
  7. LOOP 1 (repair): fix the code, verify by running it on the platform as a system user
  8. LOOP 2 (hint): diff → hint, guardrail model (different family) must explicitly approve
  9. deliver: store hint, status=done
```

Every step writes an `events` row; every model call writes an `llm_calls` row.

## Go layout

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
research/              frozen Python prototypes (reference only, see research/README.md)
```

## Conventions

- **TDD**: write failing tests first, implement until green, refactor. Every task in the plan
  includes new/updated tests — no exceptions.
- Agent loops are tested with **scripted models** (no API key needed), same pattern as the
  Python prototypes in `research/`.
- Platform calls are tested against a **mock platform** implementation + `httptest` fixtures;
  only Task 15 (ejudge) talks to anything real.
- Store tests run against a **real, dockerized Postgres**; isolation approach is documented at
  the top of `internal/store/store_test.go`.
- Run `go test ./...` **and** `golangci-lint run` after every task — both, every time, before
  moving on.
- Gate on the irreversible action (delivering a hint), not on writing one. Checker failure ≠
  approval. The guardrail model must be a **different model family** than the writer that
  produced the hint, and approval must be explicit (prose, wrong schema, or a dead connection
  are never approval).
- Deterministic checks run before any model call — a hopeless hint or a hopeless shield case
  costs zero tokens.
- Both-directions testing for judges/guardrails: at least one case that must be caught/rejected
  and one that must pass/be approved.
- `docs/` is tracked in git — do not re-add it to `.gitignore`.

## Patterns discovered during implementation

- **Store tests isolate via per-test transaction, not per-test schema**: `TestMain` runs
  migrations once against `TEST_DATABASE_URL`, then each test opens its own transaction on the
  shared pool, binds a `Store` to it (`store.WithTx`), and rolls back on cleanup — safe for
  `t.Parallel()`, no per-test DB churn. See the header comment in
  `internal/store/store_test.go`.
- **The transaction trick does not isolate packages from each other**: `go test ./...` runs
  package binaries concurrently against the *same* `TEST_DATABASE_URL`, and
  `internal/worker`'s claim-race test has to commit a pending row (a claim race can't be
  observed inside one transaction). Queue queries — `ClaimNext`, `Heartbeat`, `ReclaimStale` —
  range over the whole `help_requests` table rather than one row named by argument, so that
  committed row breaks `internal/store`'s queue tests intermittently. Both packages take a
  Postgres advisory lock (`lockQueueTable`, `queueLockKey`, defined in each package's test
  file and kept in sync) around the affected tests. A new test that calls a queue query against
  the real store needs that lock.
- **Platform mock supports error-scripting**, not just canned success responses
  (`mock.ScriptStatusError`, `mock.ScriptSubmitError` in `internal/platform/mock`) — needed to
  exercise "platform goes down mid-loop" and get it routed to `status=failed` rather than a bare
  Go error bubbling out of `RunPipeline`.
- **Resume is step-level, not attempt-level**: a crash resumes the pipeline at the last
  completed *step* (`help_requests.resume_step`); a crash strictly inside an in-flight
  repair/hint loop attempt re-enters that attempt from scratch on resume. This is intentional
  MVP scope, documented in `internal/worker/pipeline.go`'s header comment — don't "fix" it
  without checking the plan's "Resume granularity" decision first.
- **`agents.yaml` validation is strict by construction**: `yaml.Decoder.KnownFields(true)`
  rejects unknown keys at both the top level and inside each agent block; every agent's `model`
  must have a matching `pricing` entry or `config.Load` fails at startup — a misconfigured or
  renamed model fails loudly before serving traffic, never silently at first LLM call. The
  guardrail's model must also be a different family than the hint writer's (`modelFamily` in
  `internal/config/config.go`, compared on the id's *last* path segment up to its first `-`, so a
  routing prefix can't disguise one model as two): the different-family rule under Conventions is
  enforced at startup, not merely documented. `internal/config/config_test.go` also parses the
  real checked-in `agents.yaml`, so a bad edit to that file fails `go test ./...` directly. The
  `defaults` block is validated for the same reason its agents are: both its fields fail *open*
  when zero — `n_submissions: 0` removes the cap on how many submissions a request scrapes, and
  `daily_requests_per_user: 0` makes the rate limiter 429 every request — so a missing or
  mistyped key has to fail at startup, not at first traffic.
- **Cost caps are checked at two different points on purpose** (`max_cost_per_loop` before each
  attempt starts, `max_cost_per_retry` against *one attempt's* spend, not the loop's running
  total) because a call's cost is only known after it returns — both caps can overshoot by at
  most one call; see "Cost caps" in the plan for the exact enforcement points if you touch
  `internal/agent/*`. The per-retry cap's enforcement point differs by loop: `repair` checks it
  between tool-loop calls within an attempt; `hint` has no tool loop, so it checks after the
  writer call and, when hit, abandons the attempt before the guardrail call — the last point at
  which spending can still be avoided. The guardrail's own cost therefore counts toward
  `max_cost_per_loop` but never toward `max_cost_per_retry`.
- **The curator's `max_retries` is slack, not a total call budget**: `curator.Run`'s budget is
  one call per raw mistake in the batch *plus* `max_retries`, because each raw mistake may need
  its own `merge_into`/`create_mistake` before `finish`. Sizing it from `max_retries` alone made
  any batch larger than the cap permanently unprocessable — `finish` is never reached, so the
  batch is never marked processed and every later sweep re-sends an ever-growing batch. Because
  that budget scales with the batch, and the whole batch is inlined into every call's system
  prompt, `curator.maxBatchSize` bounds the batch itself; the overflow is oldest-first and
  `finish` only marks the ids in the slice, so it is simply picked up next sweep. The curator
  enforces `max_cost_per_loop` at the same point `repair` does — before each call.
- **Queue ownership is enforced in SQL, not assumed**: `Heartbeat` takes the worker id and
  matches on `claimed_by`, returning whether a row was actually refreshed. Without that
  predicate a worker whose heartbeats lapsed long enough to be reclaimed keeps the new
  claimant's row looking alive while still running the same pipeline — two workers submitting
  to the judge as the system user and spending the model budget twice, with only the final
  `TransitionStatus` detecting it, long after the side effects. A `false` return makes
  `heartbeatUntil` confirm via `GetHelpRequest` (a terminal row is the normal case) and cancel
  the run only on a genuine loss.
- **The reclaim loop is bounded by `help_requests.claim_attempts`** (`store.maxClaimAttempts`,
  incremented by `ClaimNext`): a request whose pipeline errors or panics deterministically
  never reaches a terminal status, so every sweep would hand it out again — and since steps 7-8
  are not resume-guarded, each cycle re-spends both model budgets and re-submits to the judge.
  Past the cap `ReclaimStale` moves the row to `failed` with the reason in `error` instead of
  back to `pending`.
- **Platforms name languages after the compiler, not the language**: ejudge reports its language
  `short_name` (`g++`, `gcc`, `python3`, `java8`), so `Submission.Language` almost never equals a
  `shield.Lang*` constant. `shield.Canonical` (`languageAliases` in `internal/shield/shield.go`)
  is the single mapping point — adding a platform or judge language means extending that table,
  not touching the strippers. The platform's original string is preserved for callers that need
  it back (`SubmitAsSystem` wants the `short_name`). The same naming bites the *HTML parsers*:
  a short name is not a `\w+` token (`g++`, `clang++`, `fbc-32`), and a row regex that assumes
  it is doesn't error — the row simply fails to match and vanishes from the list, which turned
  every C++ student into `no_submissions`. Column captures in
  `internal/platform/ejudge/ejudge.go` use `[^<]+` and trim, and
  `submissions_master_filtered_cpp.html` is the fixture that keeps it honest.
- **ejudge renders its error pages in the same shape as a run verdict**
  (`<h2><font color="red">…`), so parsing a verdict out of one reports a transient master
  outage as a legitimately judged failing run — the repair loop burns a retry and the request
  lands in `no_fix` instead of `failed`, collapsing exactly the distinction the pipeline exists
  to keep. `isErrorPage` gates `parseReportVerdict` and `fetchTestCounts`; add new report
  parsers behind it too.
- **Per-test data is sliced by the declared size *after* unescaping, not before**
  (`readSizedField`): ejudge's `--- Input: size N ---` counts raw bytes while the page carries
  the content HTML-escaped, so `<` occupies 4 bytes and `&` 5 — slicing the escaped text by the
  raw size truncates mid-entity and hands the repair model corrupted test data for the very
  test it is diagnosing.
- **"The judge said no" is not "our infrastructure broke"**: `platform.ErrDuplicateSubmission`
  is the backend-independent sentinel (ejudge's own error wraps it) for a judge refusing
  byte-identical code under `ignore_duplicated_runs`. That is the repair model repeating
  itself — a normal outcome — so the loop burns the retry and terminates as `no_fix`; letting
  it bubble out would put the request in `status=failed`, report an internal error to a caller
  whose request was processed exactly as designed, and pollute the failed/no_fix analytics
  split. Any new platform error that a model can *provoke* belongs on the same side of that
  line.
- **Repair verification requires the judge's own accept verdict, not just the baseline tests**
  (`success` in `internal/agent/repair/repair.go` checks `RunResult.Passed` *and* the per-test
  comparison). This course's ejudge runs `acm` scoring, which halts at the first failure, so a
  student's failing run only has test results up to that point — code passing exactly those and
  failing a later test would otherwise be delivered as a verified fix.
- **Identifiers from outside the process are validated at the boundary and re-scoped in
  queries**: `user_id` arrives from the untrusted `POST /help` body and is interpolated into an
  ejudge `filter_expr` that the judge parses server-side, where Go's `%q` is not ejudge's
  escaping — `loginRe` in `internal/platform/ejudge/ejudge.go` gates it before any request goes
  out. Identifiers that come from a *model* (the curator's `merge_into` `mistake_id`) are
  constrained by `user_id` in the SQL predicate rather than trusted, and a store miss is fed back
  to the model as a tool error instead of failing the sweep.
- **Auth is bearer-token, constant-time compare** (`subtle.ConstantTimeCompare` in
  `internal/api/api.go`), two independent tokens (`API_TOKEN` for caller-facing routes,
  `ADMIN_TOKEN` for `/admin/*`) — don't switch to `==` for "simple" token checks.
- **Analytics are Go query functions, not HTTP endpoints**: `internal/store/analytics.go`
  (`CostByRequest`, `CostByModel`, `CostByAgent`, `RequestCountsByStatus`,
  `HintEffectivenessInputs`) has no corresponding `/admin` route — call them directly or from a
  script/psql. Only `useless`/`status`/`model` filtering on `GET /admin/requests` is exposed
  over HTTP. `HintEffectivenessInputs` keys off the `hint_delivered` event, which the pipeline
  emits on the cache-hit path too (payload `{"cached": true}`) — a re-delivery is still a
  delivery, so don't add a delivery path without that event.
- **Failure detail is redacted from callers, not from operators**: `GET /requests/{id}` returns a
  fixed message for `status=failed` because `help_requests.error` holds a wrapped Go error that
  can carry provider response bodies, ejudge URLs, or DB text; the full string stays on the row,
  in the `events` log, and on `GET /admin/requests`, which is behind `ADMIN_TOKEN`.
- **`PLATFORM=mock` still requires ejudge env vars to be set** (`EJUDGE_URL`,
  `EJUDGE_SYSTEM_LOGIN`, `EJUDGE_SYSTEM_PASSWORD`) — `internal/config.LoadEnv` treats all nine
  vars as unconditionally required regardless of which platform backend is selected; this is by
  design (`newPlatform` in `cmd/helper/main.go` is the only place that branches on `PLATFORM`)
  but easy to forget when standing up a local/mock-only environment.

## Reference material

`research/` holds the original Python notebook prototypes this service is based on — read-only,
not maintained going forward. See `research/README.md` for which notebook maps to which Go
package. Where the plan and the notebooks disagree, the plan
(`docs/plans/20260729-mvp-service.md`) wins.
