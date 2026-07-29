package repair_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/repair"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/format"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/mock"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// fakeEvents is an in-memory repair.EventRecorder.
type fakeEvents struct {
	calls []eventCall
}

type eventCall struct {
	RequestID uuid.UUID
	Kind      string
	Payload   []byte
}

func (f *fakeEvents) AppendEvent(_ context.Context, id uuid.UUID, kind string, payload []byte) error {
	f.calls = append(f.calls, eventCall{id, kind, payload})
	return nil
}

// fakeMistakes is an in-memory repair.MistakeRecorder.
type fakeMistakes struct {
	calls []store.RawMistake
}

func (f *fakeMistakes) InsertRawMistake(_ context.Context, m store.RawMistake) error {
	f.calls = append(f.calls, m)
	return nil
}

func testTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("repair",
		"STATEMENT: {{problem_statement}}\nCODE: {{user_code}}\nMISTAKES: {{mistakes}}\nPREVIOUS: {{previous_code}}\n")
	if err != nil {
		t.Fatalf("parsing test template: %v", err)
	}
	return tmpl
}

func testAgent() config.AgentConfig {
	return config.AgentConfig{Model: "test-model", Temperature: 0.2, MaxRetries: 3, NTestsShown: 10}
}

func testPricing() map[string]config.PricingConfig {
	return map[string]config.PricingConfig{"test-model": {Input: 1, CachedInput: 0.1, Output: 2}}
}

func baseParams() repair.Params {
	return repair.Params{
		RequestID:        uuid.New(),
		UserID:           "user-1",
		ProblemID:        "problem-1",
		Language:         "python3",
		ProblemStatement: "print the answer",
		UserCode:         "print(0)",
	}
}

func TestRun_HappyPath(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-best", 2, platform.TestCase{Index: 2, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 2, TestsTotal: 2})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"fixed code","mistakes":[{"text":"off by one"}]}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})

	events := &fakeEvents{}
	mistakes := &fakeMistakes{}
	runner := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent(),
		Events: events, Mistakes: mistakes,
	}

	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 2

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Code != "fixed code" {
		t.Errorf("code = %q, want %q", got.Code, "fixed code")
	}
	if got.RunID != "run-1" {
		t.Errorf("run id = %q, want %q", got.RunID, "run-1")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if len(got.Mistakes) != 1 || got.Mistakes[0].Text != "off by one" {
		t.Errorf("mistakes = %+v, want one 'off by one'", got.Mistakes)
	}
	if scripted.Remaining() != 0 {
		t.Errorf("scripted responses remaining = %d, want 0", scripted.Remaining())
	}

	if len(events.calls) != 1 || events.calls[0].Kind != "repair_run_submitted" {
		t.Fatalf("events = %+v, want one repair_run_submitted", events.calls)
	}
	var payload struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(events.calls[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if payload.RunID != "run-1" {
		t.Errorf("recorded run id = %q, want %q (must be persisted before polling)", payload.RunID, "run-1")
	}

	if len(mistakes.calls) != 1 || mistakes.calls[0].Text != "off by one" {
		t.Fatalf("raw mistakes recorded = %+v, want one 'off by one'", mistakes.calls)
	}
	if mistakes.calls[0].RequestID != p.RequestID || mistakes.calls[0].UserID != p.UserID {
		t.Errorf("raw mistake not scoped to this request/user: %+v", mistakes.calls[0])
	}
}

func TestRun_ToolLoop_ListTestResultsTruncated_AndGetTest(t *testing.T) {
	plat := mock.New()
	for i, v := range []string{"OK", "WA", "WA"} {
		plat.ScriptTestCase("sub-best", i+1, platform.TestCase{Index: i + 1, Verdict: v})
	}
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 3, TestsTotal: 3})
	for i := 1; i <= 3; i++ {
		plat.ScriptTestCase("run-1", i, platform.TestCase{Index: i, Verdict: "OK", Input: "in", Expected: "out", Actual: "out"})
	}

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"list_test_results"}`, Usage: llm.Usage{InputTokens: 10}},
		llm.ScriptedResponse{JSON: `{"action":"get_test","test_id":2}`, Usage: llm.Usage{InputTokens: 10}},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`, Usage: llm.Usage{InputTokens: 10}},
	)

	agent := testAgent()
	agent.NTestsShown = 2 // fewer than the baseline's 3 tests

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 3

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (three chat calls, one attempt)", got.Attempts)
	}

	calls := scripted.Calls()
	if len(calls) != 3 {
		t.Fatalf("chat calls = %d, want 3", len(calls))
	}

	// The second call's history carries list_test_results' reply as the
	// last message; assert it was truncated to NTestsShown even though the
	// run has more tests.
	lastMsg := calls[1].Messages[len(calls[1].Messages)-1]
	var listReply struct {
		Total int `json:"total"`
		Tests []struct {
			Index   int    `json:"index"`
			Verdict string `json:"verdict"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(lastMsg.Content), &listReply); err != nil {
		t.Fatalf("unmarshal list_test_results reply: %v (content: %s)", err, lastMsg.Content)
	}
	if listReply.Total != 3 {
		t.Errorf("total = %d, want 3", listReply.Total)
	}
	if len(listReply.Tests) != 2 {
		t.Fatalf("tests listed = %d, want 2 (truncated to n_tests_shown)", len(listReply.Tests))
	}

	// The third call's history carries get_test's reply with full detail.
	getReplyMsg := calls[2].Messages[len(calls[2].Messages)-1]
	var getReply struct {
		OK   bool              `json:"ok"`
		Test platform.TestCase `json:"test"`
	}
	if err := json.Unmarshal([]byte(getReplyMsg.Content), &getReply); err != nil {
		t.Fatalf("unmarshal get_test reply: %v", err)
	}
	if !getReply.OK || getReply.Test.Index != 2 || getReply.Test.Verdict != "WA" {
		t.Errorf("get_test reply = %+v, want index 2, verdict WA", getReply)
	}
}

func TestRun_GetTest_OutOfRangeIsGracefullyReported(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"get_test","test_id":99}`},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q (out-of-range get_test must not abort the loop)", got.Status, repair.StatusFixed)
	}

	calls := scripted.Calls()
	lastMsg := calls[1].Messages[len(calls[1].Messages)-1]
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastMsg.Content), &reply); err != nil {
		t.Fatalf("unmarshal error reply: %v", err)
	}
	if reply.OK {
		t.Errorf("reply.OK = true, want false for an out-of-range test_id")
	}
}

func TestRun_RegressionIsNotSuccess_ThenFixedOnRetry(t *testing.T) {
	plat := mock.New()
	// baseline: 1,2 pass; 3,4 fail
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-best", 2, platform.TestCase{Index: 2, Verdict: "OK"})
	plat.ScriptTestCase("sub-best", 3, platform.TestCase{Index: 3, Verdict: "WA"})
	plat.ScriptTestCase("sub-best", 4, platform.TestCase{Index: 4, Verdict: "WA"})

	// attempt 1: same pass count (2/4) but a different set — test 2
	// regresses while test 3 gets newly fixed. Must NOT count as success.
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, TestsPassed: 2, TestsTotal: 4})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("run-1", 2, platform.TestCase{Index: 2, Verdict: "WA"}) // regressed
	plat.ScriptTestCase("run-1", 3, platform.TestCase{Index: 3, Verdict: "OK"}) // fixed
	plat.ScriptTestCase("run-1", 4, platform.TestCase{Index: 4, Verdict: "WA"})

	// attempt 2: everything passes.
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-2", Done: true, Passed: true, TestsPassed: 4, TestsTotal: 4})
	for i := 1; i <= 4; i++ {
		plat.ScriptTestCase("run-2", i, platform.TestCase{Index: i, Verdict: "OK"})
	}

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"attempt A","mistakes":[]}`},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"attempt B","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 4

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (attempt 1's regression must not be accepted as success)", got.Attempts)
	}
	if got.Code != "attempt B" {
		t.Errorf("code = %q, want %q", got.Code, "attempt B")
	}

	// The second attempt's system prompt must carry attempt 1's code as
	// "previous agent code".
	calls := scripted.Calls()
	sysMsg := calls[1].Messages[0]
	if sysMsg.Role != "system" || !strings.Contains(sysMsg.Content, "attempt A") {
		t.Errorf("second attempt's system prompt = %q, want it to reference attempt A's code", sysMsg.Content)
	}
}

// A judge refusing byte-identical code is the model repeating itself, not
// our infrastructure breaking. It must burn the retry and let the loop end
// as no_fix, never bubble out as a Go error — which the pipeline would turn
// into status=failed, reporting an internal error to a caller whose request
// was processed exactly as designed.
func TestRun_DuplicateSubmissionBurnsRetryInsteadOfFailingTheRun(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})

	// attempt 1: the judge rejects the code as a duplicate.
	plat.ScriptSubmitError("problem-1", fmt.Errorf("judge: %w", platform.ErrDuplicateSubmission))
	// attempt 2: genuinely fixed.
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-2", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-2", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"same as before","mistakes":[]}`},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"actually different","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v (a duplicate submission must not fail the whole run)", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the duplicate must consume a retry)", got.Attempts)
	}
	if got.Code != "actually different" {
		t.Errorf("code = %q, want %q", got.Code, "actually different")
	}
}

// The counterpart: a real platform outage must still fail the run rather
// than being swallowed as just another failed attempt.
func TestRun_NonDuplicateSubmitErrorStillFailsTheRun(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitError("problem-1", errors.New("judge: connection refused"))

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"whatever","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	if _, err := runner.Run(context.Background(), p); err == nil {
		t.Fatal("expected a platform outage to fail the run")
	}
}

func TestRun_MaxRetriesExhausted(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})

	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, TestsPassed: 0, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-2", Done: true, TestsPassed: 0, TestsTotal: 1})
	plat.ScriptTestCase("run-2", 1, platform.TestCase{Index: 1, Verdict: "WA"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"still broken A","mistakes":[]}`},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"still broken B","mistakes":[]}`},
	)

	agent := testAgent()
	agent.MaxRetries = 2
	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusNoFix {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusNoFix)
	}
	if got.Reason != repair.ReasonMaxRetries {
		t.Errorf("reason = %q, want %q", got.Reason, repair.ReasonMaxRetries)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
	if scripted.Remaining() != 0 {
		t.Errorf("scripted responses remaining = %d, want 0", scripted.Remaining())
	}
}

func TestRun_MaxCostPerLoop_StopsBeforeNextAttempt(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, TestsPassed: 0, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "WA"})

	// Usage priced so this single call's cost already meets the loop cap,
	// so a second attempt must never start.
	scripted := llm.NewScripted(nil, testPricing(), llm.ScriptedResponse{
		JSON: `{"action":"submit","code":"still broken","mistakes":[]}`, Usage: llm.Usage{InputTokens: 1},
	})

	agent := testAgent()
	agent.MaxRetries = 5
	agent.MaxCostPerLoop = 0.0000005
	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusNoFix || got.Reason != repair.ReasonCostCap {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, repair.StatusNoFix, repair.ReasonCostCap)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (loop cap must stop before a second attempt starts)", got.Attempts)
	}
	if scripted.Remaining() != 0 {
		t.Errorf("scripted responses remaining = %d, want 0", scripted.Remaining())
	}
}

func TestRun_Formatter_Enabled_FormatsCodeBeforeSubmitAndResult(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed code","mistakes":[]}`},
	)

	runner := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent(),
		Formatter: format.Runner{Enabled: true, Command: "tr a-z A-Z"},
	}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Code != "FIXED CODE" {
		t.Errorf("code = %q, want formatter output %q", got.Code, "FIXED CODE")
	}
}

func TestRun_Formatter_Failure_DoesNotKillLoop_RecordsWarning(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"still good code","mistakes":[]}`},
	)

	events := &fakeEvents{}
	runner := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent(),
		Events:    events,
		Formatter: format.Runner{Enabled: true, Command: "false"},
	}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q (a failing formatter must not kill the loop)", got.Status, repair.StatusFixed)
	}
	if got.Code != "still good code" {
		t.Errorf("code = %q, want original code preserved when the formatter fails", got.Code)
	}

	var sawWarning bool
	for _, c := range events.calls {
		if c.Kind == "formatter_failed" {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("events = %+v, want a formatter_failed event", events.calls)
	}
}

func TestRun_MaxCostPerRetry_AbortsAttemptButRetryIsUsed(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	// Attempt 1 asks for the test list; its cost alone already meets the
	// per-retry cap, so the attempt is aborted without ever submitting —
	// only one scripted response is consumed for it. Attempt 2 starts a
	// fresh conversation (fresh retry-cost budget) and succeeds.
	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"list_test_results"}`, Usage: llm.Usage{InputTokens: 1}},
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`, Usage: llm.Usage{InputTokens: 1}},
	)

	agent := testAgent()
	agent.MaxRetries = 2
	agent.MaxCostPerRetry = 0.0000005
	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the aborted attempt must still count as used)", got.Attempts)
	}
	if scripted.Remaining() != 0 {
		t.Errorf("scripted responses remaining = %d, want 0", scripted.Remaining())
	}
}

// TestRun_PassingOnlyBaselineTestsIsNotAFix pins the acm-scoring hazard: the
// judge halts at the first failure, so a student's run that died on test 3
// has a baseline covering only tests 1..3. Code that passes those but fails a
// later test must not be reported as a verified fix just because it clears
// every test the baseline happened to cover.
func TestRun_PassingOnlyBaselineTestsIsNotAFix(t *testing.T) {
	plat := mock.New()
	// baseline: judged 1,2,3 then halted on the failure at 3.
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	plat.ScriptTestCase("sub-best", 2, platform.TestCase{Index: 2, Verdict: "OK"})
	plat.ScriptTestCase("sub-best", 3, platform.TestCase{Index: 3, Verdict: "WA"})

	// attempt 1: gets further (1..4 pass) but fails at test 5, so the run is
	// not accepted. Every baseline test passes, so only Passed=false catches it.
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: false, TestsPassed: 4, TestsTotal: 5})
	for i := 1; i <= 4; i++ {
		plat.ScriptTestCase("run-1", i, platform.TestCase{Index: i, Verdict: "OK"})
	}
	plat.ScriptTestCase("run-1", 5, platform.TestCase{Index: 5, Verdict: "WA"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"passes more tests, still wrong","mistakes":[]}`},
	)

	agent := testAgent()
	agent.MaxRetries = 1
	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 3

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusNoFix {
		t.Fatalf("status = %q, want %q (a run the judge did not accept is not a fix)", got.Status, repair.StatusNoFix)
	}
	if got.Code != "" {
		t.Errorf("code = %q, want empty (unverified code must never be returned)", got.Code)
	}
}

func TestRun_NoBaselineTestResults_ShortCircuitsBeforeAnyModelCall(t *testing.T) {
	plat := mock.New()
	// No ScriptTestCase for sub-best and BaselineTestsTotal 0: the student's
	// run reported no per-test results at all.

	// Zero scripted responses: any Chat call panics, which is the assertion
	// that no model call and no platform submission happen.
	scripted := llm.NewScripted(nil, testPricing())

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 0

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusNoFix || got.Reason != repair.ReasonNoBaseline {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, repair.StatusNoFix, repair.ReasonNoBaseline)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", got.Attempts)
	}
}

func TestRun_PollsUntilVerificationRunIsDone(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: false})
	// Two "still judging" polls, then the terminal verdict.
	plat.ScriptRunResultSequence("run-1",
		platform.RunResult{ID: "run-1", Done: false},
		platform.RunResult{ID: "run-1", Done: false},
		platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1},
	)
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	p.PollInterval = time.Millisecond

	got, err := runner.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.RunID != "run-1" {
		t.Errorf("run id = %q, want run-1", got.RunID)
	}
}

func TestRun_VerificationRunNeverDone_TimesOut(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: false})
	// Sticky last entry: the run stays wedged in the judge queue forever.
	plat.ScriptRunResultSequence("run-1", platform.RunResult{ID: "run-1", Done: false})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	p.PollInterval = time.Millisecond
	p.MaxPollWait = 20 * time.Millisecond

	_, err := runner.Run(context.Background(), p)
	if !errors.Is(err, repair.ErrVerificationTimeout) {
		t.Fatalf("err = %v, want wrapping ErrVerificationTimeout", err)
	}
}

func TestRun_PollingHonorsContextCancellation(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: false})
	plat.ScriptRunResultSequence("run-1", platform.RunResult{ID: "run-1", Done: false})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	p.PollInterval = time.Minute // long enough that cancellation, not the tick, wins

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.Run(ctx, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want wrapping context.Canceled", err)
	}
}

func TestRun_MistakeProfileRendersIntoPrompt(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"submit","code":"fixed","mistakes":[]}`},
	)

	runner := &repair.Runner{Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent()}
	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	p.Mistakes = []string{"- off-by-one: loops one past the end (seen 3 times)"}

	if _, err := runner.Run(context.Background(), p); err != nil {
		t.Fatalf("Run: %v", err)
	}

	system := scripted.Calls()[0].Messages[0].Content
	if !strings.Contains(system, "MISTAKES: - off-by-one: loops one past the end (seen 3 times)") {
		t.Errorf("mistake profile did not reach the prompt; system message:\n%s", system)
	}
}

// fakeRuns is an in-memory repair.RunRecorder.
type fakeRuns struct {
	calls []struct {
		RequestID uuid.UUID
		Code      string
		RunID     string
	}
}

func (f *fakeRuns) SetRepairResult(_ context.Context, id uuid.UUID, _, code, runID string) error {
	f.calls = append(f.calls, struct {
		RequestID uuid.UUID
		Code      string
		RunID     string
	}{id, code, runID})
	return nil
}

// The plan requires the verification run id persisted BEFORE polling, so a
// crash in the polling window resumes by polling rather than re-submitting.
// An events row does not satisfy that — nothing reads it back — so the id
// (and the code it carries, which loop 2 needs) goes on the request row.
func TestRun_PersistsRunIDBeforePolling(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptSubmitResult("problem-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-1", 1, platform.TestCase{Index: 1, Verdict: "OK"})

	scripted := llm.NewScripted(nil, testPricing(), llm.ScriptedResponse{
		JSON:  `{"action":"submit","code":"fixed code"}`,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
	})
	runs := &fakeRuns{}
	r := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t),
		Agent: testAgent(), Runs: runs,
	}

	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	got, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if len(runs.calls) != 1 {
		t.Fatalf("SetRepairResult calls = %d, want 1 (persisted at submit time)", len(runs.calls))
	}
	if runs.calls[0].RunID != "run-1" || runs.calls[0].Code != "fixed code" {
		t.Errorf("persisted = %+v, want run-1 / \"fixed code\"", runs.calls[0])
	}
}

// The crash-resume case itself: handed a run id it already submitted, the
// loop polls that run instead of submitting again. Re-submitting would spend
// a second model budget and put another run under the shared system login,
// once per reclaim.
func TestRun_PendingRunIsPolledNeverResubmitted(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})
	plat.ScriptRunResult("run-7", platform.RunResult{ID: "run-7", Done: true, Passed: true, TestsPassed: 1, TestsTotal: 1})
	plat.ScriptTestCase("run-7", 1, platform.TestCase{Index: 1, Verdict: "OK"})
	// No ScriptSubmitResult: the mock panics on an unscripted call, so a
	// re-submission fails this test loudly.

	// No scripted replies either — a model call would panic for the same
	// reason.
	scripted := llm.NewScripted(nil, testPricing())
	r := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: testAgent(),
	}

	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1
	p.PendingRunID = "run-7"
	p.PendingCode = "code from before the crash"

	got, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusFixed {
		t.Fatalf("status = %q, want %q", got.Status, repair.StatusFixed)
	}
	if got.RunID != "run-7" {
		t.Errorf("RunID = %q, want run-7 (the resumed run)", got.RunID)
	}
	if got.Code != "code from before the crash" {
		t.Errorf("Code = %q, want the code persisted alongside the run", got.Code)
	}
	if scripted.Remaining() != 0 {
		t.Errorf("scripted replies left = %d, want 0", scripted.Remaining())
	}
}

// A model that cannot produce schema-shaped JSON even after llm.Chat's own
// retry is the model failing, not our infrastructure: the attempt is burned
// and the loop terminates as no_fix. Bubbling the error out would report an
// internal error for a request processed exactly as designed and pollute the
// failed/no_fix analytics split.
func TestRun_InvalidModelResponseBurnsRetryNotTheRequest(t *testing.T) {
	plat := mock.New()
	plat.ScriptTestCase("sub-best", 1, platform.TestCase{Index: 1, Verdict: "WA"})

	agent := testAgent()
	agent.MaxRetries = 2
	scripted := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}, Err: llm.ErrInvalidResponse},
		llm.ScriptedResponse{Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}, Err: llm.ErrInvalidResponse},
	)
	r := &repair.Runner{
		Chat: scripted, Platform: plat, Template: testTemplate(t), Agent: agent,
	}

	p := baseParams()
	p.BaselineRunID = "sub-best"
	p.BaselineTestsTotal = 1

	got, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != repair.StatusNoFix || got.Reason != repair.ReasonMaxRetries {
		t.Errorf("status/reason = %q/%q, want no_fix/max_retries", got.Status, got.Reason)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (each unusable reply burns one)", got.Attempts)
	}
}
