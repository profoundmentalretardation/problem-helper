# Problem Helper — MVP Service

A Go service that helps students with programming homework **without talking to them directly**.
A frontend layer (Telegram bot, web, anything) calls this HTTP API with `user_id` + `problem_id`;
the service pulls the student's submissions from a judging platform (ejudge first), finds the
defect in their best failing attempt, verifies a fix by actually running it on the platform as a
system user, and returns a non-obvious hint that teaches instead of giving the answer away.

See `AGENTS.md` for the pipeline diagram, package layout, and conventions. The full spec,
decisions, and task breakdown live in `docs/plans/20260729-mvp-service.md`.

## Running locally

Requires Go 1.26+, Docker (for Postgres), and an OpenAI-compatible LLM endpoint.

```sh
docker compose up -d          # starts Postgres on localhost:5432 (user/pass/db: helper)
cp .env.example .env          # fill in LLM_BASE_URL / LLM_API_KEY, see below
export $(cat .env | xargs)    # or use direnv / your shell's env loader
make build
make run                      # runs ./helper, applies migrations on startup, listens on :8080
```

`make test` runs `go test ./...` (store tests need `TEST_DATABASE_URL`, default
`postgres://helper:helper@localhost:5432/helper?sslmode=disable`, pointed at the same
docker-compose Postgres). `make lint` runs `golangci-lint run`. Both are required to pass before
any change is considered done.

The binary takes flags for non-default paths/address:

```sh
./helper -agents agents.yaml -prompts prompts -addr :8080 -shutdown-timeout 30s
```

## Configuration

### Environment variables (all required)

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres connection string |
| `LLM_BASE_URL`, `LLM_API_KEY` | OpenAI-compatible endpoint for repair/hint/guardrail/curator calls |
| `PLATFORM` | `ejudge` (production) or `mock` (local/dev, no real judge needed) |
| `API_TOKEN` | bearer token for `POST /help` and `GET /requests/{id}` |
| `ADMIN_TOKEN` | bearer token for `/admin/*` |
| `EJUDGE_URL`, `EJUDGE_SYSTEM_LOGIN`, `EJUDGE_SYSTEM_PASSWORD` | ejudge system-user credentials (required even when `PLATFORM=mock`; see `internal/config`) |
| `EJUDGE_CONTEST_ID` | ejudge contest the sessions are scoped to; optional, defaults to `1`. Both ejudge sessions are per-contest, so a course on any other contest id needs this set — otherwise the client finds no runs and every request answers `no_submissions`. |
| `WORKER_CONCURRENCY` | how many help requests this instance runs at once; optional, defaults to `1`. One request holds its slot for the whole repair loop — every verification submit and the judge polling that follows it — so a course-sized queue wants more than one. |

### `agents.yaml` (checked in, validated at startup)

Per-agent model, temperature, retry/cost caps, and per-1M-token pricing for every configured
model — see the checked-in `agents.yaml` for the current values and `internal/config/config.go`
for validation rules (all four agent keys and a pricing entry per configured model are required;
unknown keys are rejected). `guardrail.model` must also be a **different model family** than
`hint.model` — families are compared on the model id's last path segment up to its first `-`, so
`gpt-4.1-mini` vs `gpt-4o` is rejected while `gpt-4.1-mini` vs `claude-sonnet-5` is accepted. A
model asked to review its own output is not an independent check, so the service refuses to start
rather than let a config edit degrade the gate to self-approval.

### Prompts

`prompts/*.md` hold the system prompts (`repair`, `hint`, `guardrail`, `curator`) with
`{{placeholder}}` templating, loaded and validated at startup — see `internal/prompt`.

## API

All endpoints require `Authorization: Bearer <token>` (`API_TOKEN` for the first two,
`ADMIN_TOKEN` for `/admin/*`).

- `POST /help` `{user_id, problem_id, n_submissions?}` → `202 {request_id}`. Enqueues the
  pipeline; a worker picks it up asynchronously. Rate-limited per user per day
  (`daily_requests_per_user` in `agents.yaml`). The body is capped at 8 KiB, `user_id` and
  `problem_id` at 128 characters each, and `n_submissions` must be in `1..200`; violations
  return `400` with an `error` field.
- `GET /requests/{id}` → current `status` plus a status-specific field: `hint` (done),
  `message` (already_solved / no_submissions / no_fix / no_hint), or `error` (failed). The
  `error` field is a fixed, non-specific message: `help_requests.error` holds a wrapped Go error
  that can carry provider response bodies, ejudge URLs, or DB text, so the detail is not exposed
  to callers. Operators read it from `GET /admin/requests` or from the row and the `events` log.
- `POST /admin/metaloop/run` → runs the curator sweep on demand (same work the nightly cron
  does) and returns a summary (`users_processed`, `merged`, `created`, `gave_up`).
- `POST /admin/requests/{id}/useless` → flags a delivered hint as unhelpful, independent of
  pipeline status.
- `GET /admin/requests?useless=&status=&model=` → filterable request listing. Includes the full
  `error` string for failed requests (the detail redacted from the caller-facing route).

### Operational notes

- A verification run in the repair loop is polled every 2 s and given at most 5 minutes of
  wall clock before the attempt gives up, so a run wedged in the judge queue can't pin a worker
  slot (with `Concurrency=1` that would halt the service, and the heartbeat keeps the reclaim
  sweep from freeing it).
- A submission whose baseline run reports no per-test results at all short-circuits to `no_fix`
  with `failure_reason=no_baseline`, before any model call or verification submission.

Analytics (cost per request/model/agent, request counts by status, hint-effectiveness inputs)
are exposed as Go query functions in `internal/store/analytics.go` rather than HTTP endpoints —
call them from `psql`/a script, or wire an endpoint if a consumer needs one.

## Repository layout

See `AGENTS.md` → "Go layout" for the package breakdown, and `research/README.md` for how the
frozen Python prototypes map onto the Go packages that replaced them.
