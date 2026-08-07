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

- The pipeline is a LangGraph state machine in `graph.py`. Both loops are edges, not `for`
  statements, so every iteration is a checkpoint — that is what makes `/resume` cheap.
  Adding a step means adding a node plus its routing function, never a loop inside a node.
- `orchestrator.process_session` / `resume_session` are the only entry points of the
  background task and the only places that write session status. Both funnel into `_run`,
  which swallows exceptions into `internal_error`, so a crashed session never stays stuck
  in `running`.
- Graph nodes are pure functions of `PipelineState`: they reach the model through
  `LLMProtocol` and the sandbox through the injected `runner`, and they never touch the
  database or the settings object. The orchestrator watches `astream(stream_mode=
  ["updates", "values"])` and turns updates into stage changes and attempt rows — that is
  what keeps the whole pipeline testable without a provider.
- State lives in `state.PipelineState` only; no ad-hoc dicts between nodes. The two attempt
  logs append through `operator.add` and `research` through `add_messages`, so a streamed
  update carries exactly what the node just produced.
- Code runs through an injected `runner: Callable[[str], Awaitable[TestReport]]` built by
  `orchestrator.make_runner`. `sandbox.run_tests` is the single swap point between the two
  backends; keep that signature stable and keep both backends returning the same
  `TestReport`, or the graph above them means different things per deployment.
- Hint retries deliberately clear the conversation (`RemoveMessage(REMOVE_ALL_MESSAGES)`)
  and rebuild a fresh context with the rejected hints and the validator's remarks appended,
  instead of continuing it. That is a token-cost decision, not an oversight.
- `research → tools → research` is the only edge the model controls. Never hardcode a tool
  call: bind the tools and follow what comes back. `MAX_TOOL_ROUNDS` caps the loop, and the
  cap is enforced *after* the ToolNode so the conversation never ends on an unanswered
  tool call.
- The reading list attached to a hint is rebuilt from the tool results
  (`tools.read_materials`) and intersected with the ids the model named, so a hallucinated
  id cannot reach the student.

## Sandbox

- Two backends behind `sandbox.run_tests`: `docker` (default) and `local`. There is
  deliberately **no `auto`** — a host whose daemon is down would then silently run untrusted
  code under the weaker isolation, and that mistake is invisible because everything keeps
  working. `ensure_ready` raises `SandboxUnavailable` once, before the first test, and the
  orchestrator finishes the session as `sandbox_unavailable`.
- The container flags in `sandbox/container._run_args` are the isolation. A flag deleted by
  an editing accident is a silent loss of a security property, which is why
  `test_run_args_carry_every_isolation_flag` asserts on the command line itself.
- The `local` backend exists so the suite runs without a container runtime, and nothing
  selects it implicitly. Orchestrator tests pin it; sandbox tests parametrise over both.

## Safety

- Four layers, and the numbering in `safety/__init__.py` is the reading order:
  1 input filtering (`safety/inputs.py`), 2 structural separation (`safety/channels.py`),
  3 output filtering (`safety/outputs.py`), 4 capability constraints — the code shield, the
  container, and the fact that no registered tool can write, fetch or execute anything.
- Layer 2 is the one that holds when layer 1's patterns miss, so **every untrusted field
  reaches a prompt through `safety.fence`** and every system prompt ends with
  `safety.FENCE_RULE`. Adding a field to a prompt means fencing it; interpolating a user
  string directly is the bug `prompts.py` is arranged to make visible.
- Guardrails are graph nodes, not wrappers. `screen` makes a refused request a terminal
  state with an error code rather than an exception; `screen_hint` routes a blocked hint
  into the *existing* hint retry loop through `_rejection`, which is also what the
  validator's own rejections use — there is one definition of what a rejection costs. The
  fixer's code is screened inside `verify`, so a refusal is a failing attempt the fix loop
  already knows how to retry.
- The code shield is tuned for a low false-positive rate, not for completeness: a
  legitimate solution refused as hostile is a worse failure than a hostile one the container
  catches instead. `open(0)` and `os.read(0, …)` are fast-input idioms and must keep
  passing; unparsable code is allowed through because a `SyntaxError` is the service's main
  use case and cannot execute anything.
- `GuardConfig` switches exist for the attack suite's ablation, not as a supported way to
  run the service.

## Tracing

- One trace per session. `mlflow.autolog()` covers everything that goes through LangChain;
  `@mlflow.trace` covers the three things it cannot see — the session root span, the
  sandbox, and the guardrails.
- Tags are the taught vocabulary: `request_origin` is `api` / `ui` / `batch` and is checked
  against that list rather than passed through, and `eval_case_id` is set only on batch
  runs, where the case set is bounded.
- The tools are traced twice on purpose (autolog's span plus ours, which pins
  `gen_ai.tool.name`). `evals.trajectory` keeps only the outermost tool span of a nested
  chain, so a call is never double-counted. Do not "fix" the duplication by removing the
  decorator — the extractor would then depend on how the integration names its spans.
- `tracing.configure` is idempotent and process-wide. `tests/conftest.py` calls it with
  `enabled=False` at import time, before any decorator runs, which is what keeps the suite
  from writing a store into the repository.
- The tracking URI is a database (`sqlite:///mlflow.db`). MLflow 3 refuses `./mlruns`
  outright, and `search_traces` / `log_feedback` need a real backend.

## Retrieval

- The corpus is `src/problem_helper/corpus/*.md`, one file per material. The `##` headings
  are the chunk boundary, so they are part of the data format — rewriting a note into one
  long section changes what retrieval can find. `materials.py` parses the frontmatter and
  never searches; `retrieval/` indexes the sections it hands out.
- Chunk ids (`{material_id}#{section}.{part}`) are positional and move whenever the
  chunking parameters change. Nothing may store them: the eval set anchors on
  `(material_id, heading)` and resolves ids at load time.
- `RetrievalService.search` returns **retriever order**. `pack_for_lim` reorders for a
  model's attention and runs only in `tools.py`; scoring a packed list silently degrades
  MRR and nDCG while hit rate, precision and recall stay put, which is what makes the
  mistake invisible.
- `rerank: bool` is a parameter of one pipeline, not a second code path — that is what lets
  the harness measure the configuration the service actually runs.
- The models are loaded lazily and the unit suite never loads them: `conftest.stub_retriever`
  installs a `StubRetriever` for every test. Chunking, BM25, RRF and packing are pure and
  tested directly; the dense index and the reranker are covered by the eval harness.

## Evaluation

- `evals/` imports `problem_helper` and is never imported by it. It is not packaged, and
  `pythonpath = ["."]` in the pytest config is what lets the tests import it.
- The five rank-aware metrics return `None`, never `0.0`, for a case with an empty golden
  set, and `aggregate` reports how many were skipped. Out-of-corpus cases are data.
- Both the judgements *and* the answers under test go through `ResponseCache`. Caching only
  the judgements does not make a run reproducible — the answering model is sampled, and a
  new answer is a new judge prompt.
- The judge model must differ from the answering model; `run_generation.py` exits rather
  than let a model grade itself.
- Committed numbers in the README come from `evals/results/`. Re-run the runners rather
  than editing a table by hand.
- The agent eval and the attack suite both run in **two phases**: execute, then score stored
  traces. Nothing is measured inline. That is what lets the same scorers run over production
  traffic, and it is the reason `evals/trace_scorers.py` and `evals/safety_scorer.py` take a
  trace rather than a pipeline.
- The scorer bodies do not change to accommodate a trace. `evals/trajectory.py` is the whole
  adapter layer; if wiring a scorer to a trace ever requires editing the scorer, the adapter
  is doing too little.
- The eval raises the temperature (`--temperature`, default 0.7) and the service does not.
  Three runs at 0.0 are three copies of one run, and `pass^3` would be identically equal to
  `pass@1`.
- `expected_tools` in `agent_cases.json` is a set of acceptable trajectories. When a real run
  takes a sensible plan the file did not list, widen the file — that is a gap in the eval
  set, not an agent regression. Tightening the metric instead is how an eval suite becomes a
  test of its author's imagination.
- `evals/agent_cases.json` anchors on samples by id, never by copying the task and tests, for
  the same reason `cases.json` anchors on `(material_id, heading)`.

## LLM access

Everything goes through `langchain_openai.ChatOpenAI`; the raw `openai` package is imported
only for the error types. `LLMClient` exposes two calls and nothing else — `structured`
(JSON answer validated by a pydantic schema, optionally continuing an earlier `history`)
and `chat` (one turn with tools bound). Nodes depend on `LLMProtocol`, never on the class.

`llm.strict_schema` hardens a pydantic model for strict `json_schema` mode: it forces
`additionalProperties: false` and marks every property required. Structured-output models
in `schemas.py` (`Mistake`, `FixResult`, `HintResult`, `ValidationResult`) must therefore
declare no optional fields and no defaults. Providers that reject `json_schema` fall back
to schema-in-prompt automatically, and the fallback is remembered per model id for the
lifetime of the client.

## Tests

- `pytest` runs with `asyncio_mode = auto`, so async tests need no decorator.
- `tests/conftest.py` holds `FakeLLM`, which serves canned answers keyed by schema type for
  `structured` and canned `AIMessage`s for `chat` (both repeat the last one), `tool_turn()`
  for scripting a tool call, and `report()`, which builds a synthetic `TestReport` without
  spawning processes. Use them instead of touching the network.
- `FakeLLM.chat` hands out a fresh copy of each scripted answer: reusing a message object
  would make `add_messages` treat the second append as an edit of the first.
- Graph tests use `InMemorySaver` for the checkpoint/resume cases; the sqlite saver only
  appears in the app itself.
- Orchestrator tests run the real sandbox, pinned to the `local` backend: they cover the
  streaming-to-rows path, and `tests/test_sandbox.py` covers the container.
- The false-positive halves of `test_safety.py` and `test_codeshield.py` are the load-bearing
  ones. Both files list legitimate inputs chosen to sit next to a detector — a virtual
  machine that "ignores all previous instructions", `open(0)`, a task about environment
  files. Two of those cases failed on first run and the filters were loosened, not the tests.
- API tests are sync and use `TestClient` plus
  `create_app(settings, db=..., processor=..., resumer=...)` injection; background work is
  observed by polling `GET /v1/sessions/{id}`.
- `schemas.TestCase` carries `__test__ = False` so pytest does not collect it as a class.

## Tooling and ops

- `uv run pytest`; `uvx ruff check src evals tests` — ruff is not a project dependency.
- The suite needs `docker pull python:3.13-alpine`. Without it the container tests skip
  rather than fail, and the rest of the suite is unaffected.
- Smoke run against a real provider: `DB_PATH=/tmp/x.db CHECKPOINT_DB_PATH=/tmp/x-cp.db
  PORT=8079 uv run problem-helper`, then POST to `/v1/sessions` and poll. `/` serves the
  playground page, `/v1/tools` lists the registered tools and `/v1/sessions/{id}/debug`
  shows the diff, the model's code, the tool calls, the guardrail decisions and every
  attempt.
- `mlflow ui --backend-store-uri sqlite:///mlflow.db` opens the traces, with the feedback
  the scorers wrote back attached to each one.
- `uv_build` ships `src/problem_helper/static/` into the wheel, so the page works from an
  installed copy.
- `docs/` is gitignored in this repo — plans under `docs/plans/` stay local.
