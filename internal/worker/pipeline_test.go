// Test isolation: shares cache_test.go's TestMain/testPool/withStore — same
// approach as internal/store/store_test.go, real dockerized Postgres
// (TEST_DATABASE_URL), each test in its own rolled-back transaction.
package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/hint"
	"github.com/profoundmentalretardation/problem-helper/internal/agent/repair"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/mock"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// newClaimedRequest creates a help_requests row and drives it straight to
// status=running (the claim RunPipeline assumes has already happened).
func newClaimedRequest(t *testing.T, s *store.Store, ctx context.Context, nSubmissions int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:                id,
		UserID:            "user-1",
		ProblemID:         "problem-1",
		Platform:          "mock",
		NSubmissionsTaken: nSubmissions,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	if err := s.TransitionStatus(ctx, id, store.StatusRunning); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	return id
}

func testRepairTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("repair",
		"STATEMENT: {{problem_statement}}\nCODE: {{user_code}}\nMISTAKES: {{mistakes}}\nPREVIOUS: {{previous_code}}\n")
	if err != nil {
		t.Fatalf("parsing repair template: %v", err)
	}
	return tmpl
}

func testHintTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("hint", "DIFF: {{diff}}\nWORKING: {{working_code}}\n")
	if err != nil {
		t.Fatalf("parsing hint template: %v", err)
	}
	return tmpl
}

func testGuardrailTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("guardrail", "DIFF: {{diff}}\nWORKING: {{working_code}}\nHINT: {{hint}}\n")
	if err != nil {
		t.Fatalf("parsing guardrail template: %v", err)
	}
	return tmpl
}

func testPricing() map[string]config.PricingConfig {
	return map[string]config.PricingConfig{
		"repair-model":    {Input: 1, CachedInput: 0.1, Output: 2},
		"hint-model":      {Input: 1, CachedInput: 0.1, Output: 2},
		"guardrail-model": {Input: 3, CachedInput: 0.3, Output: 5},
	}
}

// newRepairRunner builds a repair.Runner wired against s (Events + Mistakes
// recorded to the real store, per this package's testing conventions) and
// chat (the writer model's scripted responses).
func newRepairRunner(t *testing.T, s *store.Store, plat platform.Platform, chat llm.ChatClient) *repair.Runner {
	t.Helper()
	return &repair.Runner{
		Chat:     chat,
		Platform: plat,
		Template: testRepairTemplate(t),
		Agent:    config.AgentConfig{Model: "repair-model", MaxRetries: 3, NTestsShown: 10},
		Events:   s,
		Mistakes: s,
	}
}

// newHintRunner builds a hint.Runner wired against writer + guardrail
// scripted clients.
func newHintRunner(t *testing.T, writer, guardrail llm.ChatClient) *hint.Runner {
	t.Helper()
	return &hint.Runner{
		Chat:              writer,
		Guardrail:         guardrail,
		Template:          testHintTemplate(t),
		GuardrailTemplate: testGuardrailTemplate(t),
		Agent:             config.AgentConfig{Model: "hint-model", MaxRetries: 3},
		GuardrailAgent:    config.AgentConfig{Model: "guardrail-model"},
	}
}

func hasEventKind(events []store.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestRunPipeline_AlreadySolved(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: true})

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusAlreadySolved {
		t.Errorf("status = %q, want %q", got.Status, store.StatusAlreadySolved)
	}

	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventKind(events, "problem_status") {
		t.Errorf("events = %+v, want a problem_status event", events)
	}
}

func TestRunPipeline_NoSubmissions(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", nil)

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusNoSubmissions {
		t.Errorf("status = %q, want %q", got.Status, store.StatusNoSubmissions)
	}
}

func TestRunPipeline_NoSubmissions_OnlyCompileErrors(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "this doesn't compile", Language: "python", TestsPassed: 0, TestsTotal: 0, SubmittedAt: time.Now()},
	})

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusNoSubmissions {
		t.Errorf("status = %q, want %q", got.Status, store.StatusNoSubmissions)
	}
}

func TestRunPipeline_UnsupportedLanguage(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "10 PRINT HELLO", Language: "basic", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	if got.Error == nil || *got.Error == "" {
		t.Errorf("error = %v, want a clear message", got.Error)
	}
}

func TestRunPipeline_PlatformErrorFetchingStatus_Failed(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatusError("user-1", "problem-1", errors.New("ejudge: connection refused"))

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	if got.Error == nil || *got.Error == "" {
		t.Errorf("error = %v, want a clear message", got.Error)
	}
}

// TestRunPipeline_PlatformErrorDuringRepairLoop_Failed verifies the "platform
// down mid-loop -> failed" edge case: a platform error surfacing from inside
// the repair loop (not just steps 1-3/5) must still land the request on
// status=failed with the error recorded, per RunPipeline's own doc comment
// and the plan's status-transition table — not left stuck running nor
// silently bubbled as an unrecorded Go error.
func TestRunPipeline_PlatformErrorDuringRepairLoop_Failed(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	plat.ScriptSubmitError("problem-1", errors.New("ejudge: connection refused"))

	repairChat := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"print(2)","mistakes":[]}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})

	pl := &worker.Pipeline{
		Store:    s,
		Platform: plat,
		Repair:   newRepairRunner(t, s, plat, repairChat),
	}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	if got.Error == nil || *got.Error == "" {
		t.Errorf("error = %v, want a clear message", got.Error)
	}
}

func TestRunPipeline_CacheHit_ZeroModelCalls(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})

	codeHash := worker.HashCode("print(1)")
	cachedRequest := newClaimedRequest(t, s, ctx, 1)
	if err := s.InsertHint(ctx, store.Hint{
		RequestID: cachedRequest,
		ProblemID: "problem-1",
		CodeHash:  codeHash,
		Text:      "think about what happens on the first iteration",
		Approved:  true,
	}); err != nil {
		t.Fatalf("insert cached hint: %v", err)
	}

	// Repair and Hint are left nil: a cache hit must never consult either
	// loop, so even touching them would fail this test loudly (nil pointer).
	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusDone)
	}
	if got.HintID == nil {
		t.Fatal("hint_id not set")
	}
	deliveredHint, err := s.GetHint(ctx, *got.HintID)
	if err != nil {
		t.Fatalf("get hint: %v", err)
	}
	if deliveredHint.Text != "think about what happens on the first iteration" {
		t.Errorf("delivered hint text = %q, want the cached hint", deliveredHint.Text)
	}

	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventKind(events, "hint_cache_hit") {
		t.Errorf("events = %+v, want a hint_cache_hit event", events)
	}
}

func TestRunPipeline_FullHappyPath(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 2, TestsTotal: 2})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "OK"})

	repairChat := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"print(2)","mistakes":[{"text":"off by one"}]}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})
	hintWriter := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"hint":"Think about what your very first print should show."}`,
		Usage: llm.Usage{InputTokens: 50, OutputTokens: 10},
	})
	guardrail := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"approved":true,"reason":"makes them think"}`,
		Usage: llm.Usage{InputTokens: 80, OutputTokens: 5},
	})

	pl := &worker.Pipeline{
		Store:    s,
		Platform: plat,
		Repair:   newRepairRunner(t, s, plat, repairChat),
		Hint:     newHintRunner(t, hintWriter, guardrail),
	}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusDone)
	}
	if got.BestSubmissionID == nil {
		t.Error("best_submission_id not set")
	}
	if got.HintID == nil {
		t.Fatal("hint_id not set")
	}
	deliveredHint, err := s.GetHint(ctx, *got.HintID)
	if err != nil {
		t.Fatalf("get hint: %v", err)
	}
	if deliveredHint.Text != "Think about what your very first print should show." {
		t.Errorf("delivered hint text = %q", deliveredHint.Text)
	}
	if !deliveredHint.Approved {
		t.Error("delivered hint not marked approved")
	}

	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, kind := range []string{
		"problem_status", "problem_statement", "best_submission_picked", "shield_applied",
		"repair_run_submitted", "repair_result", "hint_result", "hint_delivered",
	} {
		if !hasEventKind(events, kind) {
			t.Errorf("events = %+v, want a %q event", events, kind)
		}
	}

	if repairChat.Remaining() != 0 {
		t.Errorf("repair chat: %d scripted responses unconsumed", repairChat.Remaining())
	}
	if hintWriter.Remaining() != 0 {
		t.Errorf("hint writer: %d scripted responses unconsumed", hintWriter.Remaining())
	}
	if guardrail.Remaining() != 0 {
		t.Errorf("guardrail: %d scripted responses unconsumed", guardrail.Remaining())
	}
}

func TestRunPipeline_NoFix(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	// Every verification attempt still fails test 2, so the repair loop
	// exhausts its (MaxRetries: 3) budget.
	for _, runID := range []string{"run-1", "run-2", "run-3"} {
		plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: runID, Done: true, Passed: false, TestsPassed: 1, TestsTotal: 2})
		plat.ScriptTestCase(runID, 1, platform.TestCase{Index: 1, Verdict: "OK"})
		plat.ScriptTestCase(runID, 2, platform.TestCase{Index: 2, Verdict: "WA"})
	}

	repairChat := llm.NewScripted(s, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"attempt 1","mistakes":[]}`, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"attempt 2","mistakes":[]}`, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"attempt 3","mistakes":[]}`, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}},
	)

	pl := &worker.Pipeline{
		Store:    s,
		Platform: plat,
		Repair:   newRepairRunner(t, s, plat, repairChat),
	}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusNoFix {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusNoFix)
	}
	if got.FailureReason == nil || *got.FailureReason != "max_retries" {
		t.Errorf("failure_reason = %v, want %q", got.FailureReason, "max_retries")
	}
}

func TestRunPipeline_NoHint(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 2, TestsTotal: 2})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "OK"})

	repairChat := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"print(2)","mistakes":[]}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})
	// The guardrail never approves, so the hint loop exhausts its
	// (MaxRetries: 3) budget without a different hint proposed each time.
	hintWriter := llm.NewScripted(s, testPricing(),
		llm.ScriptedResponse{JSON: `{"hint":"hint attempt one"}`, Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: `{"hint":"hint attempt two"}`, Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: `{"hint":"hint attempt three"}`, Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
	)
	guardrail := llm.NewScripted(s, testPricing(),
		llm.ScriptedResponse{JSON: `{"approved":false,"reason":"too explicit"}`, Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: `{"approved":false,"reason":"too explicit"}`, Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: `{"approved":false,"reason":"too explicit"}`, Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
	)

	pl := &worker.Pipeline{
		Store:    s,
		Platform: plat,
		Repair:   newRepairRunner(t, s, plat, repairChat),
		Hint:     newHintRunner(t, hintWriter, guardrail),
	}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusNoHint {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusNoHint)
	}
	if got.FailureReason == nil || *got.FailureReason != "max_retries" {
		t.Errorf("failure_reason = %v, want %q", got.FailureReason, "max_retries")
	}
	if got.HintID != nil {
		t.Error("hint_id set despite no_hint outcome")
	}
}

func TestRunPipeline_RespectsNSubmissionsLimit(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 1)

	plat := mock.New()
	plat.ScriptStatus("user-1", "problem-1", platform.Status{Solved: false})
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmissions("user-1", "problem-1", []platform.Submission{
		{ID: "sub-old", Code: "print(1)", Language: "python", TestsPassed: 2, TestsTotal: 2, SubmittedAt: time.Now().Add(-time.Hour)},
		{ID: "sub-new", Code: "print(2)", Language: "python", TestsPassed: 0, TestsTotal: 2, SubmittedAt: time.Now()},
	})
	plat.ScriptTestCase("sub-old", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-old", 2, platform.TestCase{Index: 2, Verdict: "OK"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 2, TestsTotal: 2})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "OK"})

	codeHash := worker.HashCode("print(1)")
	cachedRequest := newClaimedRequest(t, s, ctx, 1)
	if err := s.InsertHint(ctx, store.Hint{
		RequestID: cachedRequest, ProblemID: "problem-1", CodeHash: codeHash, Text: "cached hint", Approved: true,
	}); err != nil {
		t.Fatalf("insert cached hint: %v", err)
	}

	pl := &worker.Pipeline{Store: s, Platform: plat}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	subs, err := s.ListSubmissions(ctx, id)
	if err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	if len(subs) != 1 || subs[0].PlatformSubmissionID != "sub-old" {
		t.Errorf("submissions = %+v, want exactly [sub-old] (n_submissions_taken=1 truncates to the first scripted submission)", subs)
	}
}

// TestRunPipeline_ResumesAtCheckpoint_SkipsCompletedSteps simulates a
// worker crash-reclaimed after the shield step: the request is claimed
// (running), submissions and the shield record already committed to the
// store, resume_step="shield". Re-running the pipeline must not re-fetch
// the problem status or submissions (unscripted, so the mock would panic)
// and must not insert duplicate submissions/shield rows — only the steps
// after the checkpoint (cache miss onward) should execute.
func TestRunPipeline_ResumesAtCheckpoint_SkipsCompletedSteps(t *testing.T) {
	s, ctx := withStore(t)
	id := newClaimedRequest(t, s, ctx, 10)

	plat := mock.New()
	// Deliberately NOT scripted: ProblemStatus, Submissions. A resumed run
	// past StepSubmissions must never call either.
	plat.ScriptStatement("problem-1", platform.Statement{ProblemID: "problem-1", Title: "t", Text: "solve it"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 2, TestsTotal: 2})
	plat.ScriptTestCase("sub-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-1", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "OK"})

	// Pre-populate exactly what steps 3-5 (submissions, pick, shield) would
	// have committed before the simulated crash.
	subID := uuid.New()
	if err := s.SnapshotSubmissions(ctx, id, []store.Submission{
		{ID: subID, PlatformSubmissionID: "sub-1", Code: "print(1)", Language: "python", TestsPassed: 1, TestsTotal: 2, SubmittedAt: time.Now(), IsBest: true},
	}); err != nil {
		t.Fatalf("pre-seed submissions: %v", err)
	}
	if err := s.SetBestSubmission(ctx, id, subID); err != nil {
		t.Fatalf("pre-seed best submission: %v", err)
	}
	if err := s.InsertShieldRecord(ctx, store.ShieldRecord{
		RequestID: id, CodeBefore: "print(1)", CodeAfter: "print(1)", Diff: "",
	}); err != nil {
		t.Fatalf("pre-seed shield record: %v", err)
	}
	if err := s.SetResumeStep(ctx, id, worker.StepShield); err != nil {
		t.Fatalf("pre-seed resume step: %v", err)
	}

	repairChat := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"print(2)","mistakes":[]}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})
	hintWriter := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"hint":"Think about what your very first print should show."}`,
		Usage: llm.Usage{InputTokens: 50, OutputTokens: 10},
	})
	guardrail := llm.NewScripted(s, testPricing(), llm.ScriptedResponse{
		JSON:  `{"approved":true,"reason":"makes them think"}`,
		Usage: llm.Usage{InputTokens: 80, OutputTokens: 5},
	})

	pl := &worker.Pipeline{
		Store:    s,
		Platform: plat,
		Repair:   newRepairRunner(t, s, plat, repairChat),
		Hint:     newHintRunner(t, hintWriter, guardrail),
	}
	if err := pl.RunPipeline(ctx, id); err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusDone)
	}

	subs, err := s.ListSubmissions(ctx, id)
	if err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	if len(subs) != 1 {
		t.Errorf("submissions = %+v, want exactly the one pre-seeded row (resume must not re-snapshot)", subs)
	}

	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if hasEventKind(events, "problem_status") {
		t.Errorf("events = %+v, want no problem_status event on a resumed run past StepStatus", events)
	}
	if hasEventKind(events, "best_submission_picked") {
		t.Errorf("events = %+v, want no best_submission_picked event on a resumed run past StepSubmissions", events)
	}
}
