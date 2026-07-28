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
`docs/plans/20260729-mvp-service.md` (or `docs/plans/completed/` once done) — this file is the
orientation layer, that plan is the source of truth.

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

## Reference material

`research/` holds the original Python notebook prototypes this service is based on — read-only,
not maintained going forward. See `research/README.md` for which notebook maps to which Go
package. Where the plan and the notebooks disagree, the plan
(`docs/plans/20260729-mvp-service.md`) wins.
