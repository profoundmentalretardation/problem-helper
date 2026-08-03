# problem-helper

An agent service: it takes a problem statement, a student's broken Python code and tests
(`stdin → stdout`), and returns a **hint** rather than a ready solution.

The repaired code and the diff stay inside the service — the agents need them so the hint
aims at the real mistake, and they are exposed only through the debug endpoint.

## How it works

```
POST /v1/sessions ──► session row (pending) ──► asyncio task
                                                  │
                       1. run the student's code in the sandbox (baseline)
                          └─ everything green → outcome=already_correct, done
                       2. FIX LOOP (FIXER_MODEL), up to MAX_FIX_ATTEMPTS:
                          agent → {mistakes[], fixed_code} → run the tests
                          └─ still red at the end → failed / fix_failed
                       3. unified diff (student's code ↔ model's code)
                       4. HINT LOOP, up to MAX_HINT_ATTEMPTS:
                          HINT_MODEL writes the hint → VALIDATOR_MODEL judges it
                          (accuracy / explicitness / no spoiler / language)
                          └─ rejected → regenerate with the remarks in context
                          └─ rejected to the end → failed / hint_rejected
                       5. succeeded: hint and mistake list stored
```

Every iteration of both loops is appended to the `attempts` table, so you can see what the
model proposed and why the validator killed a hint.

The prompts are written in English, and each agent is told to produce its student-facing
text in the language of the problem statement — a Russian task yields a Russian hint.

## Running

```bash
cp .env.example .env      # put your LLM_API_KEY in
uv sync
uv run problem-helper     # http://127.0.0.1:8000
```

## Web playground

`GET /` serves a single-file page (`src/problem_helper/static/index.html`, no build step,
no CDN, no auth) for driving the service by hand: statement, code, an editable list of
tests, the two attempt limits, live stage while polling, then the hint and the mistake
list. "Show internals" pulls `/debug` and renders the diff, the model's solution, the
student's code against every test, and each fix/hint attempt with the validator's remarks.
The page prefills a broken even-sum solution, so it is one click to see the whole loop.
The session id goes into the URL hash, so reloading `/#<session_id>` reopens a session.

## API

### `POST /v1/sessions` → `202`

```json
{
  "task": "Given N integers, print the sum of the even ones.",
  "code": "n = int(input())\n...",
  "tests": [
    {"input": "5\n1 2 3 4 5\n", "expected_output": "6"}
  ],
  "max_fix_attempts": 3,
  "max_hint_attempts": 3
}
```

Response: `{"session_id": "...", "status": "pending"}`. Processing runs in the background.

### `GET /v1/sessions/{id}` — what may be shown to the student

```json
{
  "session_id": "...",
  "status": "succeeded",
  "stage": "done",
  "result": {
    "outcome": "hint_ready",
    "hint": "On line 5 the condition `nums[i] % 2 == 1` selects odd numbers...",
    "mistakes": [{"title": "...", "detail": "...", "line": 5}],
    "tests_total": 3,
    "tests_passed_before": 0
  },
  "error": null
}
```

`status`: `pending → running → succeeded | failed`, `stage`: `queued → running_tests →
fixing → hinting → done`.

`error.code`: `fix_failed` (no working solution in N attempts), `hint_rejected` (the
validator never approved a hint), `internal_error`.

### `GET /v1/sessions/{id}/debug` — for the teacher

The original request, `fixed_code`, the diff, test reports and every attempt of both loops.

## Configuration

Everything via `.env` (see `.env.example`). The important knobs:

| Variable | Default | Meaning |
|---|---|---|
| `LLM_BASE_URL` | `https://openrouter.ai/api/v1` | Any OpenAI-compatible provider |
| `FIXER_MODEL` | `anthropic/claude-sonnet-4.5` | Mistake analysis and code repair |
| `HINT_MODEL` | `google/gemini-3.5-flash-lite` | Hint generation (the cheap one) |
| `VALIDATOR_MODEL` | `google/gemini-3.5-flash` | Hint judge |
| `MAX_FIX_ATTEMPTS` / `MAX_HINT_ATTEMPTS` | 3 / 3 | Loop limits |
| `SANDBOX_TIMEOUT_SEC` / `SANDBOX_MEMORY_MB` | 5 / 256 | Code execution limits |
| `DB_PATH` | `problem_helper.db` | SQLite file |

Structured output goes through `response_format=json_schema`; if a model cannot do it, the
client automatically falls back to putting the schema in the prompt.

## Sandbox

Student and model code run in a separate `python -I` process in its own process session:
temporary cwd, stripped env, `RLIMIT_AS/CPU/FSIZE/CORE`, kill on timeout (children
included), stdout/stderr truncation. There is no network ban and no namespace isolation —
in production `sandbox.run_tests` is swapped for a container run behind the same interface.

## Development

```bash
uv run pytest          # 42 tests, the LLM is replaced by a fake
uvx ruff check src tests
```

Modules: `sandbox` (execution), `llm` (structured output), `fixer` / `hinter` (the loops),
`orchestrator` (wiring and statuses), `db` (aiosqlite), `api` (FastAPI + the static page).

## MVP limitations

- one language — Python;
- SQLite, and background work lives in the same process (a restart loses unfinished sessions);
- no authentication and no rate limits;
- tests are `stdin → stdout` only.
