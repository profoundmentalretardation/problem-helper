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
`docs/plans/completed/20260729-mvp-service.md` — this file is the orientation layer, that plan
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
  renamed model fails loudly before serving traffic, never silently at first LLM call.
  `internal/config/config_test.go` also parses the real checked-in `agents.yaml`, so a bad edit
  to that file fails `go test ./...` directly.
- **Cost caps are checked at two different points on purpose** (`max_cost_per_loop` before each
  attempt starts, `max_cost_per_retry` between tool-loop calls within an attempt) because a
  call's cost is only known after it returns — both caps can overshoot by at most one call; see
  "Cost caps" in the plan for the exact enforcement points if you touch `internal/agent/*`.
- **Auth is bearer-token, constant-time compare** (`subtle.ConstantTimeCompare` in
  `internal/api/api.go`), two independent tokens (`API_TOKEN` for caller-facing routes,
  `ADMIN_TOKEN` for `/admin/*`) — don't switch to `==` for "simple" token checks.
- **Analytics are Go query functions, not HTTP endpoints**: `internal/store/analytics.go`
  (`CostByRequest`, `CostByModel`, `CostByAgent`, `RequestCountsByStatus`,
  `HintEffectivenessInputs`) has no corresponding `/admin` route — call them directly or from a
  script/psql. Only `useless`/`status`/`model` filtering on `GET /admin/requests` is exposed
  over HTTP.
- **`PLATFORM=mock` still requires ejudge env vars to be set** (`EJUDGE_URL`,
  `EJUDGE_SYSTEM_LOGIN`, `EJUDGE_SYSTEM_PASSWORD`) — `internal/config.LoadEnv` treats all nine
  vars as unconditionally required regardless of which platform backend is selected; this is by
  design (`newPlatform` in `cmd/helper/main.go` is the only place that branches on `PLATFORM`)
  but easy to forget when standing up a local/mock-only environment.

## Reference material

`research/` holds the original Python notebook prototypes this service is based on — read-only,
not maintained going forward. See `research/README.md` for which notebook maps to which Go
package. Where the plan and the notebooks disagree, the plan
(`docs/plans/completed/20260729-mvp-service.md`) wins.
