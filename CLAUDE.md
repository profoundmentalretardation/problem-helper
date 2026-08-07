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
  `orchestrator.make_runner`. `sandbox.run_tests` is the single swap point for moving
  execution into a container; keep that signature stable.
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
- Committed numbers in the README come from `evals/results/`. Re-run the two runners rather
  than editing a table by hand.

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
- Orchestrator tests run the real sandbox — subprocess execution is fast enough and worth
  covering end to end.
- API tests are sync and use `TestClient` plus
  `create_app(settings, db=..., processor=..., resumer=...)` injection; background work is
  observed by polling `GET /v1/sessions/{id}`.
- `schemas.TestCase` carries `__test__ = False` so pytest does not collect it as a class.

## Tooling and ops

- `uv run pytest`; `uvx ruff check src tests` — ruff is not a project dependency.
- Smoke run against a real provider: `DB_PATH=/tmp/x.db CHECKPOINT_DB_PATH=/tmp/x-cp.db
  PORT=8079 uv run problem-helper`, then POST to `/v1/sessions` and poll. `/` serves the
  playground page, `/v1/tools` lists the registered tools and `/v1/sessions/{id}/debug`
  shows the diff, the model's code, the tool calls and every attempt.
- `uv_build` ships `src/problem_helper/static/` into the wheel, so the page works from an
  installed copy.
- `docs/` is gitignored in this repo — plans under `docs/plans/` stay local.
