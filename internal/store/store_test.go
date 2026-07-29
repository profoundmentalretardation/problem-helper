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

func TestSetResumeStep(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetResumeStep(ctx, id, "shield"); err != nil {
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
	err := s.SetResumeStep(ctx, uuid.New(), "shield")
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

	if err := s.SetBestSubmission(ctx, id, subID); err != nil {
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
	err := s.SetBestSubmission(ctx, uuid.New(), uuid.New())
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

	if err := s.SetHintID(ctx, id, hintID); err != nil {
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
	err := s.SetHintID(ctx, uuid.New(), uuid.New())
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetFailureReason(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetFailureReason(ctx, id, "max_retries"); err != nil {
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
	err := s.SetFailureReason(ctx, uuid.New(), "max_retries")
	if !errors.Is(err, store.ErrUnknownRequest) {
		t.Errorf("err = %v, want wrapping ErrUnknownRequest", err)
	}
}

func TestSetError(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SetError(ctx, id, "platform unreachable"); err != nil {
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
	err := s.SetError(ctx, uuid.New(), "platform unreachable")
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
	if err := s.MergeMistake(ctx, id); err != nil {
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
	err := s.MergeMistake(ctx, uuid.New())
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
	if err := s.MergeMistake(ctx, topCount); err != nil {
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
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if err := s.TransitionStatus(ctx, id, store.StatusRunning); err != nil {
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
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}

	if err := s.Heartbeat(ctx, id); err != nil {
		t.Fatalf("heartbeat: %v", err)
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
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx) // still pending, never claimed

	if err := s.Heartbeat(ctx, id); err != nil {
		t.Fatalf("heartbeat on a non-running row should be a silent no-op, got: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.HeartbeatAt != nil {
		t.Errorf("heartbeat_at = %v, want nil (heartbeat must not touch a non-running row)", got.HeartbeatAt)
	}
}

func TestReclaimStale_MovesStaleRunningRowToPending_PreservesResumeStep(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if err := s.SetResumeStep(ctx, id, "shield"); err != nil {
		t.Fatalf("set resume step: %v", err)
	}

	reclaimed, err := s.ReclaimStale(ctx, time.Now().Add(time.Hour))
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
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)
	if _, err := s.ClaimNext(ctx, "worker-1"); err != nil {
		t.Fatalf("claim next: %v", err)
	}

	reclaimed, err := s.ReclaimStale(ctx, time.Now().Add(-time.Hour))
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
