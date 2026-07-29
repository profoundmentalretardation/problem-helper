// Test isolation: these tests run against a real, dockerized Postgres
// (see docker-compose.yml), reachable via TEST_DATABASE_URL (default
// postgres://helper:helper@localhost:5432/helper?sslmode=disable).
//
// Migrations run exactly once, in TestMain, against that database. Each
// individual test then opens its own transaction on the shared pool, binds
// a Store to that transaction (store.WithTx), and rolls the transaction
// back in a deferred cleanup. Tests therefore never observe each other's
// writes and never commit anything — no per-test schema or database churn
// is needed, and tests can run with t.Parallel() safely.
package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://helper:helper@localhost:5432/helper?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		panic(err)
	}
	if err := pool.Ping(ctx); err != nil {
		panic("store_test: cannot reach test postgres at " + dsn + ": " + err.Error())
	}
	if err := store.Migrate(ctx, pool); err != nil {
		panic(err)
	}
	testPool = pool

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// withStore begins a transaction on the shared test pool, binds a Store to
// it, and rolls back on test cleanup.
func withStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
	})

	return store.WithTx(tx), ctx
}

// queueLockKey guards the tests whose assertions range over the whole
// help_requests table — ClaimNext/Heartbeat/ReclaimStale all pick "some
// pending/running row" rather than one named by argument, so a committed row
// from anywhere else in the database breaks them.
//
// The per-test transaction isolates this package from itself, but not from
// other packages: `go test ./...` runs package binaries concurrently against
// the same TEST_DATABASE_URL, and internal/worker's TestWorker_ClaimNext_*
// deliberately commits a pending row (a claim race can't be observed inside
// one transaction). Both sides take this advisory lock, so they interleave
// instead of overlapping. Keep the key in sync across packages.
const queueLockKey = 0x70726F626C656D01

// lockQueueTable takes queueLockKey for the duration of the test on its own
// connection, so it is not tied to the test's rolled-back transaction.
func lockQueueTable(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn for queue lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(queueLockKey)); err != nil {
		conn.Release()
		t.Fatalf("take queue lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(queueLockKey))
		conn.Release()
	})
}

func createRequest(t *testing.T, s *store.Store, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:                id,
		UserID:            "user-1",
		ProblemID:         "problem-1",
		Platform:          "mock",
		NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	return id
}

func TestCreateHelpRequest(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusPending {
		t.Errorf("status = %q, want %q", got.Status, store.StatusPending)
	}
	if got.UserID != "user-1" || got.ProblemID != "problem-1" || got.Platform != "mock" {
		t.Errorf("unexpected row: %+v", got)
	}
	if got.NSubmissionsTaken != 5 {
		t.Errorf("n_submissions_taken = %d, want 5", got.NSubmissionsTaken)
	}
}

func TestCreateHelpRequest_Duplicate(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:        id,
		UserID:    "user-2",
		ProblemID: "problem-2",
		Platform:  "mock",
	})
	if err == nil {
		t.Fatal("expected error on duplicate id, got nil")
	}
	if !errors.Is(err, store.ErrDuplicateRequest) {
		t.Errorf("err = %v, want wrapping ErrDuplicateRequest", err)
	}
}

func TestTransitionStatus_LegalPath(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.TransitionStatus(ctx, id, store.StatusRunning, ""); err != nil {
		t.Fatalf("pending -> running: %v", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusDone, ""); err != nil {
		t.Fatalf("running -> done: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Errorf("status = %q, want %q", got.Status, store.StatusDone)
	}
}

func TestTransitionStatus_ReclaimPath(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.TransitionStatus(ctx, id, store.StatusRunning, ""); err != nil {
		t.Fatalf("pending -> running: %v", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusPending, ""); err != nil {
		t.Fatalf("running -> pending (reclaim): %v", err)
	}
}

func TestTransitionStatus_Illegal(t *testing.T) {
	tests := []struct {
		name string
		from store.Status
		to   store.Status
	}{
		{"pending directly to done", store.StatusPending, store.StatusDone},
		{"pending to itself", store.StatusPending, store.StatusPending},
		{"terminal done to running", store.StatusDone, store.StatusRunning},
		{"terminal failed to anything", store.StatusFailed, store.StatusPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx := withStore(t)
			id := createRequest(t, s, ctx)

			if tt.from != store.StatusPending {
				// drive the row to `from` via the legal pending->running->from
				// path (running is an intermediate for every non-pending
				// terminal status in this graph).
				if err := s.TransitionStatus(ctx, id, store.StatusRunning, ""); err != nil {
					t.Fatalf("setup pending -> running: %v", err)
				}
				if tt.from != store.StatusRunning {
					if err := s.TransitionStatus(ctx, id, tt.from, ""); err != nil {
						t.Fatalf("setup running -> %s: %v", tt.from, err)
					}
				}
			}

			err := s.TransitionStatus(ctx, id, tt.to, "")
			if err == nil {
				t.Fatalf("transition %s -> %s: expected error, got nil", tt.from, tt.to)
			}
			var illegal *store.ErrIllegalTransition
			if !errors.As(err, &illegal) {
				t.Errorf("err = %v, want *ErrIllegalTransition", err)
			} else {
				// From comes from the atomic statement's pre-UPDATE
				// snapshot; a regression there would otherwise be silent.
				if illegal.From != tt.from {
					t.Errorf("illegal.From = %q, want %q", illegal.From, tt.from)
				}
				if illegal.To != tt.to {
					t.Errorf("illegal.To = %q, want %q", illegal.To, tt.to)
				}
			}

			got, getErr := s.GetHelpRequest(ctx, id)
			if getErr != nil {
				t.Fatalf("get help request: %v", getErr)
			}
			if got.Status != tt.from {
				t.Errorf("status changed despite illegal transition: got %q, want unchanged %q", got.Status, tt.from)
			}
		})
	}
}

func TestTransitionStatus_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.TransitionStatus(ctx, uuid.New(), store.StatusRunning, "")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetResumeStep(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetResumeStep(ctx, id, "", "shield"); err != nil {
		t.Fatalf("set resume step: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.ResumeStep == nil || *got.ResumeStep != "shield" {
		t.Errorf("resume_step = %v, want %q", got.ResumeStep, "shield")
	}
}

func TestSetResumeStep_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetResumeStep(ctx, uuid.New(), "", "shield")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

// The row mutators the pipeline calls mid-run are claim-scoped for the same
// reason TransitionStatus is: a worker that was reclaimed while still
// executing steps must not write onto the new claimant's row. Both
// directions are covered — the claimant's own write lands, a stranger's is
// refused with ErrClaimLost — because a predicate that rejects everything
// would pass a one-sided test while breaking every pipeline.
func TestSetters_ClaimScoping(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}

	byOwner := map[string]func(worker string) error{
		"SetResumeStep":     func(w string) error { return s.SetResumeStep(ctx, id, w, "shield") },
		"SetRepairResult":   func(w string) error { return s.SetRepairResult(ctx, id, w, "code", "run-1") },
		"SetBestSubmission": func(w string) error { return s.SetBestSubmission(ctx, id, w, uuid.New()) },
		"SetHintID":         func(w string) error { return s.SetHintID(ctx, id, w, uuid.New()) },
		"SetFailureReason":  func(w string) error { return s.SetFailureReason(ctx, id, w, "max_retries") },
		"SetError":          func(w string) error { return s.SetError(ctx, id, w, "boom") },
	}
	for name, set := range byOwner {
		if err := set("worker-1"); err != nil {
			t.Errorf("%s by the claimant: %v, want success", name, err)
		}
		if err := set("worker-2"); !errors.Is(err, store.ErrClaimLost) {
			t.Errorf("%s by a stranger: err = %v, want wrapping ErrClaimLost", name, err)
		}
		if err := set(""); err != nil {
			t.Errorf("%s with an empty worker id: %v, want the check skipped", name, err)
		}
	}
}

func TestSetRepairResult(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetRepairResult(ctx, id, "", "int main(void){return 0;}", "run-42"); err != nil {
		t.Fatalf("set repair result: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.RepairCode == nil || *got.RepairCode != "int main(void){return 0;}" {
		t.Errorf("repair_code = %v, want the verified fix", got.RepairCode)
	}
	if got.RepairRunID == nil || *got.RepairRunID != "run-42" {
		t.Errorf("repair_run_id = %v, want %q", got.RepairRunID, "run-42")
	}
}

func TestSetRepairResult_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetRepairResult(ctx, uuid.New(), "", "code", "run-1")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetBestSubmission(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	subID := uuid.New()
	if err := s.SnapshotSubmissions(ctx, id, []store.Submission{
		{ID: subID, PlatformSubmissionID: "sub-1", Code: "code", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now(), IsBest: true},
	}); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}

	if err := s.SetBestSubmission(ctx, id, "", subID); err != nil {
		t.Fatalf("set best submission: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.BestSubmissionID == nil || *got.BestSubmissionID != subID {
		t.Errorf("best_submission_id = %v, want %s", got.BestSubmissionID, subID)
	}
}

func TestSetBestSubmission_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetBestSubmission(ctx, uuid.New(), "", uuid.New())
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetHintID(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	hintID := uuid.New()
	if err := s.InsertHint(ctx, store.Hint{
		ID: hintID, RequestID: id, ProblemID: "problem-1", CodeHash: "hash", Text: "hint text", Approved: true,
	}); err != nil {
		t.Fatalf("insert hint: %v", err)
	}

	if err := s.SetHintID(ctx, id, "", hintID); err != nil {
		t.Fatalf("set hint id: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.HintID == nil || *got.HintID != hintID {
		t.Errorf("hint_id = %v, want %s", got.HintID, hintID)
	}
}

func TestSetHintID_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetHintID(ctx, uuid.New(), "", uuid.New())
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetFailureReason(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetFailureReason(ctx, id, "", "max_retries"); err != nil {
		t.Fatalf("set failure reason: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.FailureReason == nil || *got.FailureReason != "max_retries" {
		t.Errorf("failure_reason = %v, want %q", got.FailureReason, "max_retries")
	}
}

func TestSetFailureReason_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetFailureReason(ctx, uuid.New(), "", "max_retries")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetError(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetError(ctx, id, "", "platform unreachable"); err != nil {
		t.Fatalf("set error: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Error == nil || *got.Error != "platform unreachable" {
		t.Errorf("error = %v, want %q", got.Error, "platform unreachable")
	}
}

func TestSetError_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetError(ctx, uuid.New(), "", "platform unreachable")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestAppendEvent(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.AppendEvent(ctx, id, "shield_applied", []byte(`{"removed": 2}`)); err != nil {
		t.Fatalf("append event: %v", err)
	}

	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Kind != "shield_applied" {
		t.Errorf("kind = %q, want %q", events[0].Kind, "shield_applied")
	}
	if events[0].RequestID != id {
		t.Errorf("request_id = %v, want %v", events[0].RequestID, id)
	}
}

func TestAppendEvent_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.AppendEvent(ctx, uuid.New(), "kind", nil)
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestInsertLLMCall(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	call := store.LLMCall{
		RequestID:         id,
		Agent:             "repair",
		Model:             "gpt-test",
		InputTokens:       1000,
		CachedInputTokens: 200,
		OutputTokens:      50,
		Cost:              "0.003417",
		LatencyMS:         850,
		Attempt:           1,
		Prompt:            "prompt text",
		Response:          "response text",
	}
	if err := s.InsertLLMCall(ctx, call); err != nil {
		t.Fatalf("insert llm call: %v", err)
	}

	// find it back by re-querying via ListEvents-style direct fetch: since
	// InsertLLMCall doesn't return the generated id when unset, re-derive
	// via a fresh call with an explicit id instead.
	explicitID := uuid.New()
	call.ID = explicitID
	if err := s.InsertLLMCall(ctx, call); err != nil {
		t.Fatalf("insert llm call with explicit id: %v", err)
	}

	got, err := s.GetLLMCall(ctx, explicitID)
	if err != nil {
		t.Fatalf("get llm call: %v", err)
	}
	if got.Cost != "0.003417" {
		t.Errorf("cost = %q, want %q (numeric precision must survive the round trip)", got.Cost, "0.003417")
	}
	if got.InputTokens != 1000 || got.CachedInputTokens != 200 || got.OutputTokens != 50 {
		t.Errorf("unexpected token counts: %+v", got)
	}
}

func TestInsertLLMCall_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.InsertLLMCall(ctx, store.LLMCall{
		RequestID: uuid.New(),
		Agent:     "repair",
		Model:     "gpt-test",
		Cost:      "0",
	})
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSnapshotSubmissions(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	now := time.Now().UTC().Truncate(time.Millisecond)
	subs := []store.Submission{
		{PlatformSubmissionID: "1", Code: "print(1)", Language: "python3", TestsPassed: 3, TestsTotal: 5, SubmittedAt: now, IsBest: false},
		{PlatformSubmissionID: "2", Code: "print(2)", Language: "python3", TestsPassed: 5, TestsTotal: 5, SubmittedAt: now.Add(time.Minute), IsBest: true},
	}
	if err := s.SnapshotSubmissions(ctx, id, subs); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}

	got, err := s.ListSubmissions(ctx, id)
	if err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(submissions) = %d, want 2", len(got))
	}
	if got[1].Language != "python3" {
		t.Errorf("language = %q, want %q", got[1].Language, "python3")
	}
	if !got[1].IsBest {
		t.Errorf("expected second submission to be marked is_best")
	}
	if got[0].IsBest {
		t.Errorf("expected first submission to not be marked is_best")
	}
}

func TestSnapshotSubmissions_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SnapshotSubmissions(ctx, uuid.New(), []store.Submission{
		{PlatformSubmissionID: "1", Code: "x", Language: "python3", SubmittedAt: time.Now()},
	})
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestInsertShieldRecord(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	rec := store.ShieldRecord{
		RequestID:  id,
		CodeBefore: "int x = 1; // comment\n",
		CodeAfter:  "int x = 1; \n",
		Diff:       "- int x = 1; // comment\n+ int x = 1; \n",
		Removed:    []byte(`{"comments":["// comment"],"unicode":[],"comment_count":1,"unicode_count":0}`),
	}
	if err := s.InsertShieldRecord(ctx, rec); err != nil {
		t.Fatalf("insert shield record: %v", err)
	}

	explicitID := uuid.New()
	rec.ID = explicitID
	if err := s.InsertShieldRecord(ctx, rec); err != nil {
		t.Fatalf("insert shield record with explicit id: %v", err)
	}

	got, err := s.GetShieldRecord(ctx, explicitID)
	if err != nil {
		t.Fatalf("get shield record: %v", err)
	}
	if got.CodeBefore != rec.CodeBefore || got.CodeAfter != rec.CodeAfter || got.Diff != rec.Diff {
		t.Errorf("unexpected row: %+v", got)
	}
	if !strings.Contains(string(got.Removed), `"comment_count"`) || !strings.Contains(string(got.Removed), "// comment") {
		t.Errorf("Removed = %s, want it to round-trip the jsonb payload", got.Removed)
	}
}

func TestInsertShieldRecord_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.InsertShieldRecord(ctx, store.ShieldRecord{
		RequestID:  uuid.New(),
		CodeBefore: "x",
		CodeAfter:  "x",
	})
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestInsertRawMistake(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: id,
		UserID:    "user-1",
		Text:      "off by one in the window loop",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: id,
		UserID:    "user-1",
		Text:      "forgets to flush stdout",
	}); err != nil {
		t.Fatalf("insert second raw mistake: %v", err)
	}

	got, err := s.ListRawMistakes(ctx, id)
	if err != nil {
		t.Fatalf("list raw mistakes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d raw mistakes, want 2", len(got))
	}
	texts := map[string]bool{got[0].Text: true, got[1].Text: true}
	if !texts["off by one in the window loop"] || !texts["forgets to flush stdout"] {
		t.Errorf("unexpected raw mistakes: %+v", got)
	}
	for _, m := range got {
		if m.Processed {
			t.Errorf("raw mistake %+v: processed = true, want false (fresh row)", m)
		}
		if m.UserID != "user-1" {
			t.Errorf("user_id = %q, want %q", m.UserID, "user-1")
		}
	}
}

func TestInsertHintAndFindApprovedHint(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertHint(ctx, store.Hint{
		RequestID: id,
		ProblemID: "problem-1",
		CodeHash:  "hash-a",
		Text:      "look at your loop bound",
		Approved:  true,
	}); err != nil {
		t.Fatalf("insert hint: %v", err)
	}

	got, err := s.FindApprovedHint(ctx, "problem-1", "hash-a")
	if err != nil {
		t.Fatalf("find approved hint: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a hint")
	}
	if got.Text != "look at your loop bound" || !got.Approved {
		t.Errorf("unexpected hint: %+v", got)
	}
}

func TestFindApprovedHint_Miss(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertHint(ctx, store.Hint{
		RequestID: id,
		ProblemID: "problem-1",
		CodeHash:  "hash-a",
		Text:      "unapproved hint",
		Approved:  false,
	}); err != nil {
		t.Fatalf("insert unapproved hint: %v", err)
	}

	if got, err := s.FindApprovedHint(ctx, "problem-1", "hash-a"); err != nil || got != nil {
		t.Errorf("unapproved hint: got (%+v, %v), want (nil, nil)", got, err)
	}
	if got, err := s.FindApprovedHint(ctx, "problem-1", "hash-b"); err != nil || got != nil {
		t.Errorf("different hash: got (%+v, %v), want (nil, nil)", got, err)
	}
	if got, err := s.FindApprovedHint(ctx, "problem-2", "hash-a"); err != nil || got != nil {
		t.Errorf("different problem: got (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestInsertHint_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.InsertHint(ctx, store.Hint{
		RequestID: uuid.New(),
		ProblemID: "problem-1",
		CodeHash:  "hash-a",
		Text:      "x",
	})
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestGetHint(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	hintID := uuid.New()

	if err := s.InsertHint(ctx, store.Hint{
		ID:        hintID,
		RequestID: id,
		ProblemID: "problem-1",
		CodeHash:  "hash-a",
		Text:      "look at your loop bound",
		Approved:  true,
	}); err != nil {
		t.Fatalf("insert hint: %v", err)
	}

	got, err := s.GetHint(ctx, hintID)
	if err != nil {
		t.Fatalf("get hint: %v", err)
	}
	if got.Text != "look at your loop bound" || !got.Approved {
		t.Errorf("unexpected hint: %+v", got)
	}
}

func TestGetHint_Unknown(t *testing.T) {
	s, ctx := withStore(t)
	if _, err := s.GetHint(ctx, uuid.New()); err == nil {
		t.Error("expected error for unknown hint id")
	}
}

func TestCountRequestsSince(t *testing.T) {
	s, ctx := withStore(t)

	id1 := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID: id1, UserID: "user-rate", ProblemID: "problem-1", Platform: "mock", NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	id2 := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID: id2, UserID: "user-rate", ProblemID: "problem-2", Platform: "mock", NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	// A different user's requests must not count toward user-rate's total.
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID: uuid.New(), UserID: "someone-else", ProblemID: "problem-1", Platform: "mock", NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}

	got, err := s.CountRequestsSince(ctx, "user-rate", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2", got)
	}

	got, err = s.CountRequestsSince(ctx, "user-rate", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if got != 0 {
		t.Errorf("count with future since = %d, want 0", got)
	}

	got, err = s.CountRequestsSince(ctx, "nobody", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if got != 0 {
		t.Errorf("count for unknown user = %d, want 0", got)
	}
}

func TestInsertRawMistake_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: uuid.New(),
		UserID:    "user-1",
		Text:      "x",
	})
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestListUnprocessedRawMistakes_ScopedToUserAndUnprocessed(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertRawMistake(ctx, store.RawMistake{RequestID: id, UserID: "user-1", Text: "a"}); err != nil {
		t.Fatalf("insert raw mistake a: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{RequestID: id, UserID: "user-1", Text: "b"}); err != nil {
		t.Fatalf("insert raw mistake b: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{RequestID: id, UserID: "user-2", Text: "other user"}); err != nil {
		t.Fatalf("insert raw mistake for other user: %v", err)
	}

	got, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed raw mistakes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d unprocessed raw mistakes, want 2", len(got))
	}

	if err := s.MarkRawMistakesProcessed(ctx, []uuid.UUID{got[0].ID}); err != nil {
		t.Fatalf("mark processed: %v", err)
	}

	after, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed raw mistakes after marking: %v", err)
	}
	if len(after) != 1 || after[0].ID != got[1].ID {
		t.Fatalf("after marking one processed, got %+v, want only %s left", after, got[1].ID)
	}
}

func TestListUsersWithUnprocessedMistakes(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertRawMistake(ctx, store.RawMistake{RequestID: id, UserID: "user-a", Text: "x"}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{RequestID: id, UserID: "user-b", Text: "y"}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}
	processed := store.RawMistake{RequestID: id, UserID: "user-c", Text: "already done"}
	if err := s.InsertRawMistake(ctx, processed); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}
	all, err := s.ListUnprocessedRawMistakes(ctx, "user-c")
	if err != nil || len(all) != 1 {
		t.Fatalf("list unprocessed for user-c: %+v, %v", all, err)
	}
	if err := s.MarkRawMistakesProcessed(ctx, []uuid.UUID{all[0].ID}); err != nil {
		t.Fatalf("mark processed: %v", err)
	}

	users, err := s.ListUsersWithUnprocessedMistakes(ctx)
	if err != nil {
		t.Fatalf("list users with unprocessed mistakes: %v", err)
	}
	got := map[string]bool{}
	for _, u := range users {
		got[u] = true
	}
	if !got["user-a"] || !got["user-b"] {
		t.Errorf("users = %v, want user-a and user-b", users)
	}
	if got["user-c"] {
		t.Errorf("users = %v, user-c should be absent (its only raw mistake is processed)", users)
	}
}

func TestCreateMistakeAndMergeMistake(t *testing.T) {
	s, ctx := withStore(t)

	id := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{
		ID: id, UserID: "user-1", Title: "off-by-one", Description: "loop bound is one short",
	}); err != nil {
		t.Fatalf("create mistake: %v", err)
	}

	got, err := s.ListMistakes(ctx, "user-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("list mistakes: %+v, %v", got, err)
	}
	if got[0].Count != 1 {
		t.Errorf("count = %d, want 1", got[0].Count)
	}
	firstSeen, lastSeen := got[0].FirstSeen, got[0].LastSeen

	time.Sleep(5 * time.Millisecond)
	if err := s.MergeMistake(ctx, "user-1", id); err != nil {
		t.Fatalf("merge mistake: %v", err)
	}

	after, err := s.ListMistakes(ctx, "user-1")
	if err != nil || len(after) != 1 {
		t.Fatalf("list mistakes after merge: %+v, %v", after, err)
	}
	if after[0].Count != 2 {
		t.Errorf("count after merge = %d, want 2", after[0].Count)
	}
	if !after[0].LastSeen.After(lastSeen) {
		t.Errorf("last_seen after merge = %v, want after %v", after[0].LastSeen, lastSeen)
	}
	if !after[0].FirstSeen.Equal(firstSeen) {
		t.Errorf("first_seen changed on merge: %v -> %v", firstSeen, after[0].FirstSeen)
	}
}

func TestMergeMistake_UnknownID(t *testing.T) {
	s, ctx := withStore(t)
	err := s.MergeMistake(ctx, "user-1", uuid.New())
	if !errors.Is(err, store.ErrUnknownMistake) {
		t.Errorf("err = %v, want wrapping ErrUnknownMistake", err)
	}
}

func TestTopMistakes_OrderedByCountDescThenLastSeenDesc(t *testing.T) {
	s, ctx := withStore(t)

	older := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{ID: older, UserID: "user-1", Title: "older", Description: "d"}); err != nil {
		t.Fatalf("create older: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	newer := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{ID: newer, UserID: "user-1", Title: "newer", Description: "d"}); err != nil {
		t.Fatalf("create newer: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	topCount := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{ID: topCount, UserID: "user-1", Title: "top-count", Description: "d"}); err != nil {
		t.Fatalf("create top-count: %v", err)
	}
	if err := s.MergeMistake(ctx, "user-1", topCount); err != nil {
		t.Fatalf("merge top-count: %v", err)
	}

	got, err := s.TopMistakes(ctx, "user-1", 3)
	if err != nil {
		t.Fatalf("top mistakes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d mistakes, want 3", len(got))
	}
	want := []uuid.UUID{topCount, newer, older}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: id = %s, want %s (%+v)", i, got[i].ID, w, got)
		}
	}

	limited, err := s.TopMistakes(ctx, "user-1", 1)
	if err != nil {
		t.Fatalf("top mistakes limited: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != topCount {
		t.Fatalf("limited top mistakes = %+v, want just %s", limited, topCount)
	}
}

func TestClaimNext_ClaimsPendingRow(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	got, err := s.ClaimNext(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if got == nil {
		t.Fatal("claim next: got nil, want the pending request")
	}
	if got.ID != id {
		t.Errorf("claimed id = %s, want %s", got.ID, id)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("status = %s, want running", got.Status)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != "worker-1" {
		t.Errorf("claimed_by = %v, want worker-1", got.ClaimedBy)
	}
	if got.HeartbeatAt == nil {
		t.Error("heartbeat_at not set by claim")
	}
}

func TestClaimNext_NoPendingRows(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)

	got, err := s.ClaimNext(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil (a miss is not an error)", got)
	}
}

func TestClaimNext_SkipsAlreadyRunningRow(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if err := s.TransitionStatus(ctx, id, store.StatusRunning, ""); err != nil {
		t.Fatalf("transition to running: %v", err)
	}

	got, err := s.ClaimNext(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil (only pending rows are claimable)", got)
	}
}

func TestHeartbeat_UpdatesRunningRow(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}

	ok, err := s.Heartbeat(ctx, id, "worker-1")
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !ok {
		t.Error("heartbeat reported the row as not ours, want refreshed")
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.HeartbeatAt == nil {
		t.Error("heartbeat_at is nil after heartbeat")
	}
	if got.Status != store.StatusRunning {
		t.Errorf("status = %s, want running", got.Status)
	}
}

func TestHeartbeat_NoopWhenNotRunning(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx) // still pending, never claimed

	ok, err := s.Heartbeat(ctx, id, "worker-1")
	if err != nil {
		t.Fatalf("heartbeat on a non-running row should be a silent no-op, got: %v", err)
	}
	if ok {
		t.Error("heartbeat reported a refresh on a row that was never claimed")
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.HeartbeatAt != nil {
		t.Errorf("heartbeat_at = %v, want nil (heartbeat must not touch a non-running row)", got.HeartbeatAt)
	}
}

// A worker whose heartbeats lapsed long enough to be reclaimed must not be
// able to keep refreshing the row the new claimant now owns — otherwise
// both run the same pipeline to completion, double-submitting to the judge
// under the system account and double-spending the model budget.
func TestHeartbeat_RejectedAfterAnotherWorkerReclaims(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if _, err := s.ReclaimStale(ctx, -time.Hour); err != nil {
		t.Fatalf("reclaim stale: %v", err)
	}
	if _, err := s.ClaimNext(ctx, "worker-2"); err != nil {
		t.Fatalf("re-claim: %v", err)
	}

	ok, err := s.Heartbeat(ctx, id, "worker-1")
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if ok {
		t.Error("the old claimant's heartbeat was accepted; it must not keep worker-2's row alive")
	}

	// The new claimant's heartbeat still works — a both-directions check.
	ok, err = s.Heartbeat(ctx, id, "worker-2")
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !ok {
		t.Error("the current claimant's heartbeat was rejected")
	}
}

// A request that never reaches a terminal status (a panic, a store error, a
// corrupt checkpoint) would otherwise be reclaimed forever, re-spending the
// repair and hint budgets and re-submitting to the judge on every cycle.
func TestReclaimStale_AbandonsRequestAfterTooManyClaims(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	stale := -time.Hour
	// Each cycle claims the row and then finds it stale again, exactly as a
	// request that keeps crashing the pipeline would.
	for i := 0; i < 20; i++ {
		if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
			t.Fatalf("claim next (cycle %d): %v", i, err)
		}
		if _, err := s.ReclaimStale(ctx, stale); err != nil {
			t.Fatalf("reclaim stale (cycle %d): %v", i, err)
		}
		got, err := s.GetHelpRequest(ctx, id)
		if err != nil {
			t.Fatalf("get help request: %v", err)
		}
		if got.Status == store.StatusFailed {
			if got.Error == nil || *got.Error == "" {
				t.Error("abandoned request has no error recorded for the operator")
			}
			return
		}
	}
	t.Fatal("request was reclaimed 20 times without ever being abandoned")
}

func TestReclaimStale_MovesStaleRunningRowToPending_PreservesResumeStep(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if err := s.SetResumeStep(ctx, id, "", "shield"); err != nil {
		t.Fatalf("set resume step: %v", err)
	}

	reclaimed, err := s.ReclaimStale(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("reclaim stale: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != id {
		t.Fatalf("reclaimed = %v, want [%s]", reclaimed, id)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.ClaimedBy != nil {
		t.Errorf("claimed_by = %v, want nil", got.ClaimedBy)
	}
	if got.HeartbeatAt != nil {
		t.Errorf("heartbeat_at = %v, want nil", got.HeartbeatAt)
	}
	if got.ResumeStep == nil || *got.ResumeStep != "shield" {
		t.Errorf("resume_step = %v, want it preserved as %q", got.ResumeStep, "shield")
	}
}

func TestReclaimStale_LeavesFreshHeartbeatAlone(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}

	reclaimed, err := s.ReclaimStale(ctx, time.Hour)
	if err != nil {
		t.Fatalf("reclaim stale: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %v, want none (heartbeat is fresh)", reclaimed)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("status = %s, want still running", got.Status)
	}
}

func TestGetSubmission(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	subID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.SnapshotSubmissions(ctx, id, []store.Submission{
		{ID: subID, PlatformSubmissionID: "sub-1", Code: "print(1)", Language: "python3", TestsPassed: 4, TestsTotal: 5, SubmittedAt: now, IsBest: true},
	}); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}

	got, err := s.GetSubmission(ctx, subID)
	if err != nil {
		t.Fatalf("get submission: %v", err)
	}
	if got.PlatformSubmissionID != "sub-1" || got.Code != "print(1)" || !got.IsBest {
		t.Errorf("unexpected row: %+v", got)
	}
}

func TestGetSubmission_Unknown(t *testing.T) {
	s, ctx := withStore(t)
	if _, err := s.GetSubmission(ctx, uuid.New()); err == nil {
		t.Fatal("want error for unknown submission id")
	}
}

func TestGetShieldRecordByRequest(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if err := s.InsertShieldRecord(ctx, store.ShieldRecord{
		RequestID:  id,
		CodeBefore: "int x = 1; // c\n",
		CodeAfter:  "int x = 1; \n",
		Diff:       "diff",
	}); err != nil {
		t.Fatalf("insert shield record: %v", err)
	}

	got, err := s.GetShieldRecordByRequest(ctx, id)
	if err != nil {
		t.Fatalf("get shield record by request: %v", err)
	}
	if got.CodeAfter != "int x = 1; \n" {
		t.Errorf("code_after = %q, want %q", got.CodeAfter, "int x = 1; \n")
	}
}

func TestGetShieldRecordByRequest_Unknown(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.GetShieldRecordByRequest(ctx, id); err == nil {
		t.Fatal("want error when no shield record exists for the request")
	}
}

// TestMergeMistake_OtherUsersMistakeIsNotBumped pins the user_id predicate.
// The id reaches MergeMistake from a model that can hallucinate a well-formed
// uuid, so without the scoping a curator sweep for one student could bump a
// different student's tally.
func TestMergeMistake_OtherUsersMistakeIsNotBumped(t *testing.T) {
	s, ctx := withStore(t)

	victim := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{
		ID: victim, UserID: "user-2", Title: "theirs", Description: "d",
	}); err != nil {
		t.Fatalf("create victim mistake: %v", err)
	}

	err := s.MergeMistake(ctx, "user-1", victim)
	if !errors.Is(err, store.ErrUnknownMistake) {
		t.Fatalf("err = %v, want wrapping ErrUnknownMistake", err)
	}

	got, err := s.ListMistakes(ctx, "user-2")
	if err != nil {
		t.Fatalf("list mistakes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("mistakes = %+v, want 1", got)
	}
	if got[0].Count != 1 {
		t.Errorf("count = %d, want 1 (another user's merge must not bump it)", got[0].Count)
	}
}

// The daily cap is the only control between one caller and both the whole
// LLM budget and a flood of judge submissions under the shared system login,
// so it has to hold when a frontend bursts. Counting and inserting as two
// statements let every concurrent request read the same pre-insert count and
// all pass; the single-statement form is what makes the limit real.
func TestCreateHelpRequestWithinDailyLimit_ConcurrentBurstCannotExceedTheCap(t *testing.T) {
	// This test commits `pending` rows, which ClaimNext/ReclaimStale in both
	// this package and internal/worker will happily pick up — the same
	// cross-package hazard queueLockKey exists for.
	lockQueueTable(t)

	// A shared committed pool, not the per-test transaction: the race this
	// asserts on only exists between separate connections.
	ctx := context.Background()
	const limit = 3
	const attempts = 12
	userID := "user-burst-" + uuid.NewString()
	since := time.Now().Add(-time.Hour)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM help_requests WHERE user_id = $1`, userID)
	})

	var mu sync.Mutex
	var accepted int
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := store.New(testPool).CreateHelpRequestWithinDailyLimit(ctx, store.HelpRequestInput{
				ID: uuid.New(), UserID: userID, ProblemID: "problem-1", Platform: "mock", NSubmissionsTaken: 5,
			}, since, limit)
			if err != nil {
				t.Errorf("CreateHelpRequestWithinDailyLimit: %v", err)
				return
			}
			if created {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted > limit {
		t.Errorf("accepted = %d, want at most %d", accepted, limit)
	}
	if accepted == 0 {
		t.Errorf("accepted = 0, want the cap to admit requests, not reject everything")
	}

	var stored int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM help_requests WHERE user_id = $1`, userID).Scan(&stored); err != nil {
		t.Fatalf("counting stored rows: %v", err)
	}
	if stored != accepted {
		t.Errorf("stored rows = %d, want %d (one row per accepted request)", stored, accepted)
	}
}

// A worker whose heartbeats lapsed long enough to be reclaimed only learns
// it lost the claim on its next tick. In that window it can finish its
// pipeline and write a legal terminal status onto a row the new claimant is
// actively working — delivering the stale worker's hint and failing the
// healthy one. Ownership is checked in SQL, like Heartbeat's.
func TestTransitionStatus_RejectsAWriteFromAWorkerThatLostTheClaim(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	createRequest(t, s, ctx)

	claimed, err := s.ClaimNext(ctx, "worker-new")
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claimed == nil {
		t.Fatal("ClaimNext returned no row")
	}

	if err := s.TransitionStatus(ctx, claimed.ID, store.StatusDone, "worker-old"); !errors.Is(err, store.ErrClaimLost) {
		t.Fatalf("err = %v, want ErrClaimLost for a worker that no longer owns the row", err)
	}
	if err := s.TransitionStatus(ctx, claimed.ID, store.StatusDone, "worker-new"); err != nil {
		t.Fatalf("the current claimant must still be able to finish: %v", err)
	}
}

// The reclaim window has two halves, and the second one — reclaimed back to
// pending but not yet claimed by anybody — used to be wide open: the claim
// predicate accepted a NULL claimed_by for any worker id, so the stale worker
// that is still executing steps could walk the checkpoint backwards, stamp its
// own repair code, or fail a request that is queued for a healthy retry.
func TestClaimScoping_RejectsAStaleWorkerOnAReclaimedRow(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if _, err := s.ClaimNext(ctx, "worker-old"); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	// Reclaim it: status=pending, claimed_by=NULL, nobody claiming yet.
	if _, err := s.ReclaimStale(ctx, -time.Second); err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}

	if err := s.SetResumeStep(ctx, id, "worker-old", "shield"); !errors.Is(err, store.ErrClaimLost) {
		t.Errorf("SetResumeStep on a reclaimed row: err = %v, want wrapping ErrClaimLost", err)
	}
	if err := s.SetRepairResult(ctx, id, "worker-old", "code", "run-1"); !errors.Is(err, store.ErrClaimLost) {
		t.Errorf("SetRepairResult on a reclaimed row: err = %v, want wrapping ErrClaimLost", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusRunning, "worker-old"); !errors.Is(err, store.ErrClaimLost) {
		t.Errorf("TransitionStatus on a reclaimed row: err = %v, want wrapping ErrClaimLost", err)
	}

	// The next claimant owns it and writes normally.
	if _, err := s.ClaimNext(ctx, "worker-new"); err != nil {
		t.Fatalf("ClaimNext by the new worker: %v", err)
	}
	if err := s.SetResumeStep(ctx, id, "worker-new", "shield"); err != nil {
		t.Errorf("SetResumeStep by the new claimant: %v, want success", err)
	}
}
