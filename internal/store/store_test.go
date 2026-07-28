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

	if err := s.TransitionStatus(ctx, id, store.StatusRunning); err != nil {
		t.Fatalf("pending -> running: %v", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusDone); err != nil {
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

	if err := s.TransitionStatus(ctx, id, store.StatusRunning); err != nil {
		t.Fatalf("pending -> running: %v", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusPending); err != nil {
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
				if err := s.TransitionStatus(ctx, id, store.StatusRunning); err != nil {
					t.Fatalf("setup pending -> running: %v", err)
				}
				if tt.from != store.StatusRunning {
					if err := s.TransitionStatus(ctx, id, tt.from); err != nil {
						t.Fatalf("setup running -> %s: %v", tt.from, err)
					}
				}
			}

			err := s.TransitionStatus(ctx, id, tt.to)
			if err == nil {
				t.Fatalf("transition %s -> %s: expected error, got nil", tt.from, tt.to)
			}
			var illegal *store.ErrIllegalTransition
			if !errors.As(err, &illegal) {
				t.Errorf("err = %v, want *ErrIllegalTransition", err)
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
	err := s.TransitionStatus(ctx, uuid.New(), store.StatusRunning)
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
