package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

func TestCostByRequest(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.InsertLLMCall(ctx, store.LLMCall{
		RequestID: id, Agent: "repair", Model: "gpt-a",
		InputTokens: 100, OutputTokens: 50, Cost: "0.010000", LatencyMS: 10, Attempt: 1,
	}); err != nil {
		t.Fatalf("insert llm call: %v", err)
	}
	if err := s.InsertLLMCall(ctx, store.LLMCall{
		RequestID: id, Agent: "hint", Model: "gpt-b",
		InputTokens: 200, OutputTokens: 20, Cost: "0.020000", LatencyMS: 10, Attempt: 1,
	}); err != nil {
		t.Fatalf("insert llm call: %v", err)
	}

	got, err := s.CostByRequest(ctx, id)
	if err != nil {
		t.Fatalf("cost by request: %v", err)
	}
	want := 0.03
	f, err := parseFloat(got)
	if err != nil {
		t.Fatalf("parse cost %q: %v", got, err)
	}
	if f != want {
		t.Errorf("cost = %s, want %v", got, want)
	}
}

func TestCostByRequest_NoCalls(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	got, err := s.CostByRequest(ctx, id)
	if err != nil {
		t.Fatalf("cost by request: %v", err)
	}
	f, err := parseFloat(got)
	if err != nil {
		t.Fatalf("parse cost %q: %v", got, err)
	}
	if f != 0 {
		t.Errorf("cost = %s, want 0", got)
	}
}

func TestCostByModel(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)

	for _, c := range []store.LLMCall{
		{RequestID: id1, Agent: "repair", Model: "gpt-a", Cost: "0.010000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
		{RequestID: id2, Agent: "hint", Model: "gpt-a", Cost: "0.005000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
		{RequestID: id2, Agent: "hint", Model: "gpt-b", Cost: "0.020000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
	} {
		if err := s.InsertLLMCall(ctx, c); err != nil {
			t.Fatalf("insert llm call: %v", err)
		}
	}

	got, err := s.CostByModel(ctx)
	if err != nil {
		t.Fatalf("cost by model: %v", err)
	}
	byModel := map[string]string{}
	for _, mc := range got {
		byModel[mc.Model] = mc.Cost
	}
	fa, err := parseFloat(byModel["gpt-a"])
	if err != nil || fa != 0.015 {
		t.Errorf("gpt-a cost = %s, want 0.015 (err %v)", byModel["gpt-a"], err)
	}
	fb, err := parseFloat(byModel["gpt-b"])
	if err != nil || fb != 0.02 {
		t.Errorf("gpt-b cost = %s, want 0.02 (err %v)", byModel["gpt-b"], err)
	}
}

func TestCostByAgent(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)

	for _, c := range []store.LLMCall{
		{RequestID: id1, Agent: "repair", Model: "gpt-a", Cost: "0.010000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
		{RequestID: id2, Agent: "repair", Model: "gpt-a", Cost: "0.005000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
		{RequestID: id2, Agent: "hint", Model: "gpt-b", Cost: "0.020000", InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1},
	} {
		if err := s.InsertLLMCall(ctx, c); err != nil {
			t.Fatalf("insert llm call: %v", err)
		}
	}

	got, err := s.CostByAgent(ctx)
	if err != nil {
		t.Fatalf("cost by agent: %v", err)
	}
	byAgent := map[string]string{}
	for _, ac := range got {
		byAgent[ac.Agent] = ac.Cost
	}
	fr, err := parseFloat(byAgent["repair"])
	if err != nil || fr != 0.015 {
		t.Errorf("repair cost = %s, want 0.015 (err %v)", byAgent["repair"], err)
	}
	fh, err := parseFloat(byAgent["hint"])
	if err != nil || fh != 0.02 {
		t.Errorf("hint cost = %s, want 0.02 (err %v)", byAgent["hint"], err)
	}
}

func TestRequestCountsByStatus(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)
	id3 := createRequest(t, s, ctx)

	// id1: pending -> running -> no_fix
	if err := s.TransitionStatus(ctx, id1, store.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionStatus(ctx, id1, store.StatusNoFix); err != nil {
		t.Fatal(err)
	}
	// id2: pending -> running -> no_hint
	if err := s.TransitionStatus(ctx, id2, store.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionStatus(ctx, id2, store.StatusNoHint); err != nil {
		t.Fatal(err)
	}
	// id3: pending -> running -> failed
	if err := s.TransitionStatus(ctx, id3, store.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionStatus(ctx, id3, store.StatusFailed); err != nil {
		t.Fatal(err)
	}

	counts, err := s.RequestCountsByStatus(ctx)
	if err != nil {
		t.Fatalf("request counts by status: %v", err)
	}
	if counts[store.StatusNoFix] != 1 {
		t.Errorf("no_fix count = %d, want 1", counts[store.StatusNoFix])
	}
	if counts[store.StatusNoHint] != 1 {
		t.Errorf("no_hint count = %d, want 1", counts[store.StatusNoHint])
	}
	if counts[store.StatusFailed] != 1 {
		t.Errorf("failed count = %d, want 1", counts[store.StatusFailed])
	}
}

func TestHintEffectivenessInputs(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SnapshotSubmissions(ctx, id, []store.Submission{
		{PlatformSubmissionID: "1", Code: "a", Language: "go", TestsPassed: 1, TestsTotal: 3, SubmittedAt: time.Now()},
		{PlatformSubmissionID: "2", Code: "b", Language: "go", TestsPassed: 2, TestsTotal: 3, SubmittedAt: time.Now(), IsBest: true},
	}); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}
	if err := s.AppendEvent(ctx, id, "hint_delivered", []byte(`{"hint_id":"x"}`)); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// second request for the same user+problem, no hint delivered yet.
	id2 := createRequest(t, s, ctx)
	if err := s.SnapshotSubmissions(ctx, id2, []store.Submission{
		{PlatformSubmissionID: "3", Code: "c", Language: "go", TestsPassed: 3, TestsTotal: 3, SubmittedAt: time.Now(), IsBest: true},
	}); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}

	rows, err := s.HintEffectivenessInputs(ctx, "user-1", "problem-1")
	if err != nil {
		t.Fatalf("hint effectiveness inputs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	// help_requests.created_at is frozen to transaction start (see
	// CreateMistake's doc comment in store.go), so both requests in this
	// test transaction share one created_at — key by RequestID rather than
	// assuming a position, since ordering between ties isn't guaranteed.
	byID := map[uuid.UUID]store.HintEffectivenessRow{}
	for _, r := range rows {
		byID[r.RequestID] = r
	}
	withHint, ok := byID[id]
	if !ok || withHint.SubmissionCount != 2 {
		t.Errorf("row for %s = %+v, want 2 submissions", id, withHint)
	}
	if withHint.HintDeliveredAt == nil {
		t.Errorf("row for %s expected non-nil HintDeliveredAt", id)
	}
	withoutHint, ok := byID[id2]
	if !ok || withoutHint.SubmissionCount != 1 {
		t.Errorf("row for %s = %+v, want 1 submission", id2, withoutHint)
	}
	if withoutHint.HintDeliveredAt != nil {
		t.Errorf("row for %s expected nil HintDeliveredAt, got %v", id2, *withoutHint.HintDeliveredAt)
	}
}

// The submissions and events joins fan each other out, so a request with
// more than one hint_delivered event — a cache re-delivery, or a redelivery
// after a crash between the event and the status transition — would
// multiply its submission count if the count weren't DISTINCT.
func TestHintEffectivenessInputs_MultipleDeliveryEventsDoNotInflateCount(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	if err := s.SnapshotSubmissions(ctx, id, []store.Submission{
		{PlatformSubmissionID: "1", Code: "a", Language: "go", TestsPassed: 1, TestsTotal: 3, SubmittedAt: time.Now()},
		{PlatformSubmissionID: "2", Code: "b", Language: "go", TestsPassed: 2, TestsTotal: 3, SubmittedAt: time.Now(), IsBest: true},
	}); err != nil {
		t.Fatalf("snapshot submissions: %v", err)
	}
	for _, payload := range []string{`{"hint_id":"x"}`, `{"hint_id":"x","cached":true}`} {
		if err := s.AppendEvent(ctx, id, "hint_delivered", []byte(payload)); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}

	rows, err := s.HintEffectivenessInputs(ctx, "user-1", "problem-1")
	if err != nil {
		t.Fatalf("hint effectiveness inputs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].SubmissionCount != 2 {
		t.Errorf("SubmissionCount = %d, want 2 (two delivery events must not double it)", rows[0].SubmissionCount)
	}
	if rows[0].HintDeliveredAt == nil {
		t.Error("expected non-nil HintDeliveredAt")
	}
}

func TestSetUseless(t *testing.T) {
	s, ctx := withStore(t)
	id := createRequest(t, s, ctx)

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Useless {
		t.Fatal("expected useless=false initially")
	}

	if err := s.SetUseless(ctx, id, true); err != nil {
		t.Fatalf("set useless: %v", err)
	}

	got, err = s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Useless {
		t.Error("expected useless=true after SetUseless")
	}
}

func TestSetUseless_UnknownRequest(t *testing.T) {
	s, ctx := withStore(t)
	err := s.SetUseless(ctx, uuid.New(), true)
	if err == nil {
		t.Fatal("expected error for unknown request")
	}
}

func TestListRequests_FilterByStatus(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)
	if err := s.TransitionStatus(ctx, id1, store.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionStatus(ctx, id1, store.StatusNoFix); err != nil {
		t.Fatal(err)
	}

	status := store.StatusNoFix
	got, err := s.ListRequests(ctx, store.RequestFilter{Status: &status})
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("got %+v, want just %s", got, id1)
	}
	_ = id2
}

func TestListRequests_FilterByUseless(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	_ = createRequest(t, s, ctx)
	if err := s.SetUseless(ctx, id1, true); err != nil {
		t.Fatal(err)
	}

	useless := true
	got, err := s.ListRequests(ctx, store.RequestFilter{Useless: &useless})
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("got %+v, want just %s", got, id1)
	}
}

func TestListRequests_FilterByModel(t *testing.T) {
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)
	if err := s.InsertLLMCall(ctx, store.LLMCall{
		RequestID: id1, Agent: "repair", Model: "gpt-a", Cost: "0.01",
		InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertLLMCall(ctx, store.LLMCall{
		RequestID: id2, Agent: "repair", Model: "gpt-b", Cost: "0.01",
		InputTokens: 1, OutputTokens: 1, LatencyMS: 1, Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	model := "gpt-a"
	got, err := s.ListRequests(ctx, store.RequestFilter{Model: &model})
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("got %+v, want just %s", got, id1)
	}
}

func TestListRequests_NoFilter(t *testing.T) {
	// help_requests.created_at defaults to now(), which is frozen to
	// transaction start (see CreateMistake's doc comment in store.go for
	// the same caveat) — both requests created in this test transaction
	// share one created_at, so only set membership is asserted, not order.
	s, ctx := withStore(t)
	id1 := createRequest(t, s, ctx)
	id2 := createRequest(t, s, ctx)

	got, err := s.ListRequests(ctx, store.RequestFilter{})
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	ids := map[uuid.UUID]bool{got[0].ID: true, got[1].ID: true}
	if !ids[id1] || !ids[id2] {
		t.Fatalf("got %+v, want both %s and %s", got, id1, id2)
	}
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}
