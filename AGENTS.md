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
  repair/hint loop re-enters that whole loop on resume. This is intentional MVP scope,
  documented in `internal/worker/pipeline.go`'s header comment — don't "fix" it without
  checking the plan's "Resume granularity" decision first. The one carve-out that decision
  itself names is the verification run: `repair.Runner.Runs` persists the run id *and* the code
  it carries the moment `SubmitAsSystem` returns, and `Params.PendingRunID` makes a resumed
  loop 1 poll that run instead of submitting a second one under the shared system login. An
  events row does not satisfy this — nothing reads it back.
- **A resume checkpoint is only worth writing if the step's output is readable back.** The
  repair and hint checkpoints were originally written but never consulted, because the state
  the next step needs lived in memory: `resume_step='repair'` still re-ran loop 1 — re-spending
  both model budgets and submitting to the judge again under the system login — and the
  reclaim loop can do that up to `store.maxClaimAttempts` times. Loop 1's verified code and run
  id are now persisted (`help_requests.repair_code` / `repair_run_id`, migration 0003) *before*
  the checkpoint, and the hint checkpoint moved to *after* `InsertHint`/`SetHintID`, so
  resuming past it means "stored, only delivery left". A checkpoint past a step whose output is
  missing is treated as a corrupt row (`infraFail`), never as a licence to re-run the step.
- **`store.Migrate` holds a session advisory lock** (`migrateLockKey`): the "is this recorded?"
  check and the apply are separate statements, so two callers starting together — two instances
  rolling out, or `go test ./...` running the store and worker package binaries against the same
  `TEST_DATABASE_URL` — both see a new migration as unapplied and the loser dies at startup on
  whatever the winner already created. Adding a migration is what surfaces this; the lock is
  what keeps it from being a release-day incident.
- **Preprocessor directives are preserved but still scanned for comments**
  (`stripCLikeComments` in `internal/shield/clike.go`): copying a `#`-line through unscanned
  left `#define N 100 // payload` intact and, worse, let a `/*` opened on a directive line put
  the scanner in the wrong state so the comment's *body* on following lines was emitted as
  ordinary code — a shield bypass in the language family this course uses most. Scanning them
  also matches C itself, which removes comments in translation phase 3, before directives run
  in phase 4. `#` has no special meaning to the scanner, and `skipEscaped` stopping at newlines
  is what keeps `#error don't` from swallowing the rest of the file.
- **The C-family scanner's lexical rules are shield boundaries, not pedantry**
  (`internal/shield/clike.go`). Three separate bypasses came from approximating C's lexer:
  `u8'a'` was misread as a C++14 digit separator because `8` is a hex digit, so the literal was
  never entered and its closing quote opened a phantom one that hid the rest of the line
  (`isDigitSeparator` now walks to the token start and asks whether it begins with a digit —
  `0xFF'FF` does, `u8` does not); C++11 raw strings `R"delim(...)delim"` were scanned as
  ordinary escaped strings, so `R"(a"b)"` ended early and swallowed the following `//`, while a
  `//` *inside* a raw string was deleted from the program (`rawStringEnd`); and a `//` comment
  ending in an odd number of backslashes continues onto the next line in C's translation phase
  2, so stopping at the newline emitted comment prose to the model as code (`skipLineComment`).
  A block comment is replaced by a *space*, not deleted, for the same reason C does it —
  `int/**/main()` otherwise reaches the judge as the uncompilable `intmain()`. New literal
  forms belong in this scanner, with a case in `TestStrip_InjectionCorpus_MustNotSurvive` or
  `TestStrip_ApostropheEdgeCases`.
- **`Strict: false` on `response_format` is deliberate** (`internal/llm/client.go`): OpenAI's
  strict structured-output mode constrains which *schemas are legal* — every object must carry
  `additionalProperties: false` and list every property in `required` — and all three of this
  service's schemas are discriminated unions with optional fields. `strict: true` therefore
  400s before the model runs, i.e. every request ends `status=failed`, and no fixture catches
  it because none validate `response_format`. `Chat`'s own validate-and-retry is the enforcement.
- **A failed `Chat` still returns what it spent** (`spent` in `internal/llm/client.go`, mirrored
  by `Scripted`): both HTTP calls of a schema-rejected exchange are billed and written to
  `llm_calls`, so returning a zero `Response` on the error path let a model that keeps missing
  the schema overshoot `max_cost_per_retry`/`max_cost_per_loop` by two whole calls. Agents
  charge their caps *before* checking the error. `llm.ErrInvalidResponse` is also not infra
  failure: repair burns the attempt, hint burns the retry, curator burns a call — `no_fix` /
  `no_hint`, never `failed`.
- **Rate limiting needs a lock, and one clever statement is not enough**
  (`store.CreateHelpRequestWithinDailyLimit`): the obvious
  `INSERT ... SELECT ... WHERE (SELECT count(*) ...) < limit` looks atomic but isn't — under
  READ COMMITTED the subquery reads the statement's own snapshot, so every racing statement
  sees the same pre-insert count and all pass. The count has to run as its own statement inside
  a transaction that already holds `pg_advisory_xact_lock(hashtextextended(user_id, 0))`.
  `TestCreateHelpRequestWithinDailyLimit_ConcurrentBurstCannotExceedTheCap` is what caught the
  first, wrong fix.
- **Terminal transitions are ownership-scoped like `Heartbeat`**
  (`store.TransitionStatus`'s `workerID`, `worker.Pipeline.WorkerID`): a reclaimed worker only
  learns it lost the claim on its next heartbeat tick, and in that window it can write a
  perfectly legal terminal status onto a row the new claimant is running — delivering the stale
  worker's hint and failing the healthy one. `store.ErrClaimLost` is deliberately distinct from
  `ErrIllegalTransition`: the transition was legal, the row just isn't ours.
- **ejudge's error sentinels are read off ejudge's own markup**
  (`hasErrorTitle`/`hasErrorDetail`): report pages embed each test's stdout/stderr and source
  pages embed the whole submission, so scanning the document for `is out of range` or
  `Report is not available` let a student's text — which they author, hence can forge —
  masquerade as a judge error and push a normal failing verification into `status=failed`. The
  `<title>` is generated from ejudge's message catalogue and never carries submission text.
  That rule covers every sentinel, including the ones on the *submit* response
  (`duplicate of another run`, `Permission denied`) and on `isErrorPage` itself: the submit
  response renders the problem statement and its samples, and `isErrorPage` gates
  `hasErrorDetail` and every report parser, so a bare `strings.Contains` in either place hands
  a student (or a statement author) control of the outcome. Relatedly, a non-2xx is *never*
  ejudge (it answers its own errors with a 200 + error page), so `doOnce` treats it as
  `ErrMalformedResponse`.
- **Retryability is a property of the request, not of the failure**
  (`doWithRetry`'s `replayable` argument): `http.Client.Timeout` firing while awaiting response
  headers — the normal case for a judge that has already queued the run — is indistinguishable
  from a request that never arrived, so classifying per-failure replayed exactly the submit it
  was written to protect. Submits pass `false` and are never resent; idempotent GETs pass
  `true` and do retry a 5xx or a short read, which is what a proxy or CGI in front of ejudge
  actually returns. Transport errors are also run through `redactURLError` first: every request
  carries `SID` (an Administrator session token) and the master queries carry a student login in
  `filter_expr`, and `*url.Error` embeds the whole URL — which lands verbatim on
  `help_requests.error` and on `GET /admin/requests`.
- **Sanitize Unicode before stripping comments** (`shield.Strip`): the comment scanners match
  raw bytes, so an invisible character wedged into an opener (`/<U+200B>/ payload`) is not a
  comment to them — it fell through as ordinary code and the *later* unicode pass then handed
  the model a clean `// payload` with `Removed.Comments` empty. Order is the fix; the
  invisible-character table (`internal/shield/unicode.go`) is the other half, and it has to
  cover more than the zero-width block — word joiners, soft hyphen, variation selectors.
- **Only a triple-quoted literal alone on its logical line is a docstring**
  (`isDocstringPosition` in `internal/shield/python.go`): stripping every one of them rewrote
  `msg = """a\nb"""` to `msg = `, a `SyntaxError`. That mangled text is what the repair model
  diagnoses and what the hint's diff is taken from, so the model explained code the student
  never wrote and the "fix" went to the judge. Java text blocks are the same class of bug from
  the other side (`textBlockEnd` in `clike.go`) — without them, `//` *inside* a text block was
  deleted from the code that reaches the judge. It is language-gated because in C a bare `"""`
  is the adjacency of an empty string and an opening quote.
- **Fail-open config keys are startup errors** (`validateCaps`): every enforcement point reads a
  zero cost cap as "unlimited" and `MaxRetries` is compared as `attempts >= MaxRetries`, so
  `max_cost_per_retry`/`max_cost_per_loop`/`max_retries` at zero mean respectively no ceiling on
  model spend at all and a loop that returns `no_fix`/`no_hint` without calling a model. The
  shipped `agents.yaml` had all three at zero. Same rationale as the `defaults` block: a missing
  or mistyped key must fail before serving traffic, not silently at first call.
- **Snapshot submission ids are derived, not random** (`snapshotSubmissions` in
  `internal/worker/pipeline.go`): `SnapshotSubmissions` is idempotent via `ON CONFLICT` on
  `(request_id, platform_submission_id)`, so minting fresh random ids on a resume made every
  insert a no-op against the rows already committed — and `SetBestSubmission` then recorded an
  id present in no row, which commits silently because `best_submission_id` has no FK. The next
  resume past that checkpoint hit `GetSubmission` "no row" and errored the pipeline every cycle
  until `claim_attempts` ran out. A migration that adds a uniqueness constraint also has to
  clear the duplicates it describes first (`0004`), or it raises `23505` at startup forever.
- **Reclaim compares against the database's clock, not the app's**
  (`ReclaimStale(ctx, staleAfter)`): `heartbeat_at` is always written by `now()` on the server,
  so a cutoff computed in Go made reclaim depend on drift between two machines — fast enough and
  every live row looks stale on the first sweep, reclaimed out from under a worker still
  submitting to the judge under the shared system login; slow enough and nothing is ever
  reclaimed, so the `claim_attempts` backstop never engages either.
- **An interruption is not a verdict** (`checkGuardrail`): an unreadable guardrail *answer*
  (prose, wrong schema) is fail-closed `OK=false`, but a cancelled context or a transport
  failure is not an answer — reporting it as `no_hint`/`guardrail_failed` terminated a request
  that should have stayed reclaimable and threw away the remaining retry budget. Only
  `llm.ErrInvalidResponse` is a verdict. `llm.Chat` writes an `llm_calls` row on that path too:
  a failed call still burned the prompt, and skipping the row made a repeatedly-erroring
  provider invisible to cost analytics.
- **The curator marks its batch processed whenever it wrote something**, even on
  `StatusGaveUp` (`giveUp` in `internal/agent/curator/curator.go`): `MergeMistake` and
  `CreateMistake` commit as they go, so leaving the batch unprocessed made every later sweep
  re-send it and merge/create again — inflating `mistakes.count`, which drives the repair
  prompt's top-N, without bound. Only a sweep that wrote *nothing* leaves the batch for a retry.
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
  `max_cost_per_loop` but never toward `max_cost_per_retry`. What a cap is charged is what
  `llm.Chat` *spent*, not what its last HTTP call spent: `Chat` retries once on a
  schema-invalid reply and both calls burned tokens, so `Response.Usage`/`Cost` sum every call
  it made. Returning only the retry's figures let a model that keeps missing the schema
  overshoot both caps by a whole call while the `llm_calls` rows said otherwise.
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
- **The submit response's newest run is not necessarily ours**: every verification submits under
  one `EJUDGE_SYSTEM_LOGIN`, and `SubmitAsSystem` reads its run id off the top row of a
  "Previous submissions" table that the whole service shares. `fetchSubmitPage` now records that
  table's newest id *before* the POST and `isNewerRunID` requires the parsed id to beat it, so a
  concurrent repair of the same problem (several instances, or `Worker.Concurrency > 1`) can't
  cross-assign run ids and have `repair.success` "verify" a run that never held this code.
  Submits also go through `submitWithSession`, which re-logs in and re-posts once on
  `Error: Invalid session` — a submit lands minutes into a run, so it is the call most exposed to
  session expiry, and without that an expired session became `status=failed` on a healthy request.
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
  delivery, so don't add a delivery path without that event. Because more than one such event
  per request is therefore normal, its `submissions` count is `COUNT(DISTINCT sub.id)` — the two
  LEFT JOINs fan each other out, and a plain `COUNT` multiplies the snapshot size by the number
  of deliveries.
- **Failure detail is redacted from callers, not from operators**: `GET /requests/{id}` returns a
  fixed message for `status=failed` because `help_requests.error` holds a wrapped Go error that
  can carry provider response bodies, ejudge URLs, or DB text; the full string stays on the row,
  in the `events` log, and on `GET /admin/requests`, which is behind `ADMIN_TOKEN`.
- **`PLATFORM=mock` still requires ejudge env vars to be set** (`EJUDGE_URL`,
  `EJUDGE_SYSTEM_LOGIN`, `EJUDGE_SYSTEM_PASSWORD`) — `internal/config.LoadEnv` treats all nine
  vars as unconditionally required regardless of which platform backend is selected; this is by
  design (`newPlatform` in `cmd/helper/main.go` is the only place that branches on `PLATFORM`)
  but easy to forget when standing up a local/mock-only environment.
- **Every mid-run row write is claim-scoped, not just the terminal transition**
  (`store.claimScopedUpdate`, used by `SetResumeStep`/`SetRepairResult`/`SetBestSubmission`/
  `SetHintID`/`SetFailureReason`/`SetError`): the reclaimed-worker window that
  `TransitionStatus` and `Heartbeat` guard against is a window in which the old worker is still
  *executing steps*, so leaving the step writers unscoped let it walk the new claimant's
  checkpoint backwards (`resume_step`) — re-running loop 1 on the next resume, i.e. another
  judge submission under the shared system login and both model budgets again — and stamp its
  own `repair_code` under the claimant's `repair_run_id`, so loop 2 explained code that run
  never held. `TransitionStatus` catches none of that; it fires only after it has all
  committed. A new pipeline-called writer needs the same predicate and the same
  `worker.Pipeline.WorkerID` / `repair.Runner.WorkerID` threading.
- **A model formatting mistake is never `status=failed`, and `llm.Chat` does not check types**
  (`validateJSON` only asserts required keys are *present*): `{"hint": 42}` satisfies `Chat` and
  fails at the loop's `json.Unmarshal`. Returning that error hard-failed the request and
  reported an internal fault for a request handled exactly as designed. Every loop treats it
  the way it treats `llm.ErrInvalidResponse` — feed the error back, burn the attempt,
  terminate as `no_fix`/`no_hint`.
- **An attempt abandoned by the per-retry cost cap was never judged, so it is not "seen"**
  (`internal/agent/hint/hint.go`): recording it before the cap check banned an unjudged hint,
  so the next attempt's identical reply ended the loop as `ReasonStalled` rather than the cap
  that actually stopped it — and without a user turn before `continue`, attempt N+1 resumed a
  conversation whose last message was the model's own reply, i.e. no feedback at all.
- **A curator sweep that aborts after writing still seals its batch** (`sealBatch`/`fail` in
  `internal/agent/curator/curator.go`): `giveUp` covered the budget paths, but every *error*
  return bypassed it with merges already committed, so the next sweep re-sent the batch and
  merged again — the unbounded `mistakes.count` inflation `StatusGaveUp` exists to prevent. The
  sealing write runs on `context.WithoutCancel`, because the likeliest way to reach it is `ctx`
  itself dying (an operator disconnecting from `POST /admin/metaloop/run`, SIGTERM during the
  nightly sweep); for the same reason that handler runs the sweep on a detached context.
- **The metaloop sweeps once at startup, not only after a full interval**
  (`metaloopLoop`): a bare 24h ticker meant a service redeployed more often than once a day
  never ran the curator at all — `raw_mistakes` grew unbounded, `mistakes` stayed empty, and
  `Pipeline.TopNMistakes` always rendered nothing, silently disabling the read side of the
  whole loop. The interval timer is re-armed after each sweep rather than free-running, so a
  slow sweep cannot queue a second one behind itself.
- **`store.Migrate` runs every statement on the connection holding its lock**: going back to
  `pool` for them deadlocks any deployment whose `DATABASE_URL` caps the pool at one
  connection — `conn` is checked out for the whole call, so `pool.Exec` waits for a connection
  that frees only when `Migrate` returns. Startup hangs with no error at all.

## Reference material

`research/` holds the original Python notebook prototypes this service is based on — read-only,
not maintained going forward. See `research/README.md` for which notebook maps to which Go
package. Where the plan and the notebooks disagree, the plan
(`docs/plans/20260729-mvp-service.md`) wins.
