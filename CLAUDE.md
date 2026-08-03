# problem-helper

Agent service that turns a student's broken Python solution into a hint. README.md covers
the API and the request/response shapes; this file records the conventions and invariants
that are not obvious from reading the code.

## Language

Everything written into the repo is English: comments, docstrings, log messages, error
strings, LLM prompts, tests and docs. The prompts stay English and instruct every agent to
write its student-facing text in the language of the problem statement, so a Russian task
still yields a Russian hint — the validator checks that as one of its criteria. Non-ASCII
strings appear only as deliberate unicode round-trip fixtures in tests.

## Pipeline invariants

- `orchestrator.process_session` is the only entry point of the background task and the
  only place that writes session status. It swallows exceptions into `internal_error`, so
  a crashed session never stays stuck in `running`.
- Both loops (`fixer.run_fix_loop`, `hinter.run_hint_loop`) are pure functions of their
  arguments: they reach the model through `LLMProtocol` and report progress through
  `on_attempt`. They never touch the database or the settings object — that is what keeps
  them testable without a provider.
- The fix loop executes code through an injected
  `runner: Callable[[str], Awaitable[TestReport]]` built by `orchestrator.make_runner`.
  `sandbox.run_tests` is the single swap point for moving execution into a container; keep
  that signature stable.
- Hint retries deliberately rebuild a fresh context with the rejected hints and the
  validator's remarks appended, instead of continuing a conversation. That is a token-cost
  decision, not an oversight.

## LLM schemas

`llm.strict_schema` hardens a pydantic model for strict `json_schema` mode: it forces
`additionalProperties: false` and marks every property required. Structured-output models
in `schemas.py` (`Mistake`, `FixResult`, `HintResult`, `ValidationResult`) must therefore
declare no optional fields and no defaults. Providers that reject `json_schema` fall back
to schema-in-prompt automatically, and the fallback is remembered per model id for the
lifetime of the client.

## Tests

- `pytest` runs with `asyncio_mode = auto`, so async tests need no decorator.
- `tests/conftest.py` holds `FakeLLM`, which serves canned answers keyed by schema type and
  repeats the last one, and `report()`, which builds a synthetic `TestReport` without
  spawning processes. Use them instead of touching the network.
- Orchestrator tests run the real sandbox — subprocess execution is fast enough and worth
  covering end to end.
- API tests are sync and use `TestClient` plus `create_app(settings, db=..., processor=...)`
  injection; background work is observed by polling `GET /v1/sessions/{id}`.
- `schemas.TestCase` carries `__test__ = False` so pytest does not collect it as a class.

## Tooling and ops

- `uv run pytest`; `uvx ruff check src tests` — ruff is not a project dependency.
- Smoke run against a real provider: `DB_PATH=/tmp/x.db PORT=8079 uv run problem-helper`,
  then POST to `/v1/sessions` and poll. `/` serves the playground page and
  `/v1/sessions/{id}/debug` shows the diff, the model's code and every attempt.
- `uv_build` ships `src/problem_helper/static/` into the wheel, so the page works from an
  installed copy.
- `docs/` is gitignored in this repo — plans under `docs/plans/` stay local.
