package hint_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/hint"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
)

func testWriterTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("hint", "DIFF: {{diff}}\nWORKING: {{working_code}}\n")
	if err != nil {
		t.Fatalf("parsing writer template: %v", err)
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

func testAgent() config.AgentConfig {
	return config.AgentConfig{Model: "writer-model", Temperature: 0.7, MaxRetries: 3}
}

func testGuardrailAgent() config.AgentConfig {
	return config.AgentConfig{Model: "guardrail-model"}
}

func testPricing() map[string]config.PricingConfig {
	return map[string]config.PricingConfig{
		"writer-model":    {Input: 1, CachedInput: 0.1, Output: 2},
		"guardrail-model": {Input: 3, CachedInput: 0.3, Output: 5},
	}
}

func baseParams() hint.Params {
	return hint.Params{
		RequestID:    uuid.New(),
		OriginalCode: "for i in range(1, n - k):\n    pass\n",
		WorkingCode:  "for i in range(1, n - k + 1):\n    pass\n",
	}
}

func newRunner(writer, guardrail llm.ChatClient, t *testing.T) *hint.Runner {
	return &hint.Runner{
		Chat:              writer,
		Guardrail:         guardrail,
		Template:          testWriterTemplate(t),
		GuardrailTemplate: testGuardrailTemplate(t),
		Agent:             testAgent(),
		GuardrailAgent:    testGuardrailAgent(),
	}
}

func hintJSON(text string) string {
	return `{"hint":` + quote(text) + `}`
}

func verdictJSON(approved bool, reason string) string {
	if approved {
		return `{"approved":true,"reason":` + quote(reason) + `}`
	}
	return `{"approved":false,"reason":` + quote(reason) + `}`
}

// quote is a tiny JSON string encoder for test fixtures — no escaping
// needed for the plain-ASCII strings used here.
func quote(s string) string { return `"` + s + `"` }

func TestRun_HappyPath_RejectedOnceThenApproved(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("change the loop bound"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: hintJSON("Which window never gets scored?"), Usage: llm.Usage{InputTokens: 60, OutputTokens: 10}},
	)
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(false, "too explicit"), Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: verdictJSON(true, "makes them think"), Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusApproved {
		t.Fatalf("status = %q, want %q", got.Status, hint.StatusApproved)
	}
	if got.Hint != "Which window never gets scored?" {
		t.Errorf("hint = %q, want the second proposal", got.Hint)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].By != "model" {
		t.Errorf("rejected = %+v, want one model rejection", got.Rejected)
	}
	if writer.Remaining() != 0 || guardrail.Remaining() != 0 {
		t.Errorf("scripted responses left unused: writer=%d guardrail=%d", writer.Remaining(), guardrail.Remaining())
	}
}

func TestRun_RuleCaughtLeakNeverConsultsGuardrail(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		// The proposed code span/prescribes the edit — rules must reject
		// this before any guardrail call.
		llm.ScriptedResponse{JSON: hintJSON("Just change it to `range(1, n - k + 1)`."), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: hintJSON("Which window never gets scored?"), Usage: llm.Usage{InputTokens: 60, OutputTokens: 10}},
	)
	// Only one scripted response: if round 1 consulted the guardrail this
	// would be consumed too early and round 2 would panic on an empty
	// script — a rule-caught leak must cost zero guardrail calls.
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(true, "makes them think"), Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusApproved {
		t.Fatalf("status = %q, want %q", got.Status, hint.StatusApproved)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].By != "rules" {
		t.Errorf("rejected = %+v, want one rules rejection", got.Rejected)
	}
	if guardrail.Remaining() != 0 {
		t.Errorf("guardrail responses left = %d, want 0", guardrail.Remaining())
	}
}

func TestRun_SameHintTwiceStalls(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("same hint"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: hintJSON("same hint"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
	)
	// Only one response: proposing "same hint" again must stop the loop
	// before a second guardrail consultation, not burn another retry on it.
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(false, "too explicit"), Usage: llm.Usage{InputTokens: 80, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusNoHint || got.Reason != hint.ReasonStalled {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, hint.StatusNoHint, hint.ReasonStalled)
	}
	if guardrail.Remaining() != 0 {
		t.Errorf("guardrail responses left = %d, want 0", guardrail.Remaining())
	}
}

func TestRun_MaxRetriesExhausted(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("attempt one"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: hintJSON("attempt two"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(false, "too explicit"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: verdictJSON(false, "still too explicit"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	runner.Agent.MaxRetries = 2

	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusNoHint || got.Reason != hint.ReasonMaxRetries {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, hint.StatusNoHint, hint.ReasonMaxRetries)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
	if len(got.Rejected) != 2 {
		t.Errorf("rejected = %d, want 2", len(got.Rejected))
	}
}

func TestRun_CostCapStopsBeforeNextAttempt(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("attempt one"), Usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000}},
	)
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(false, "too explicit"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	runner.Agent.MaxRetries = 10
	runner.Agent.MaxCostPerLoop = 0.001 // the first attempt alone blows this cap

	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusNoHint || got.Reason != hint.ReasonCostCap {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, hint.StatusNoHint, hint.ReasonCostCap)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
}

// llm.validateJSON only checks that the required keys are *present*, never
// their types, so a reply like {"hint": 42} satisfies Chat and only fails
// when the loop unmarshals it. That is a model formatting mistake, not an
// infrastructure fault: it must burn the attempt and terminate as no_hint,
// never bubble out as an error (which the pipeline turns into
// status=failed and reports to the caller as an internal error).
func TestRun_WriterReplyWithWrongTypesBurnsTheAttempt(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"hint": 42}`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: `{"hint": {"text": "nested"}}`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)
	// No guardrail responses: neither malformed attempt may reach it.
	guardrail := llm.NewScripted(nil, testPricing())

	runner := newRunner(writer, guardrail, t)
	runner.Agent.MaxRetries = 2

	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v, want no_hint rather than an infrastructure failure", err)
	}
	if got.Status != hint.StatusNoHint || got.Reason != hint.ReasonMaxRetries {
		t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, hint.StatusNoHint, hint.ReasonMaxRetries)
	}
	if writer.Remaining() != 0 {
		t.Errorf("writer responses left = %d, want both attempts burned", writer.Remaining())
	}
}

// The per-retry cost cap abandons an attempt before the guardrail sees it.
// That hint was therefore never judged, so it must not be recorded as
// "seen": doing so made the next attempt's identical reply terminate the
// loop as ReasonStalled instead of letting the cap keep working, and the
// loop reported a reason that was not what actually stopped it.
func TestRun_CostPerRetryDoesNotPoisonTheNextAttempt(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("expensive hint"), Usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000}},
		llm.ScriptedResponse{JSON: hintJSON("expensive hint"), Usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000}},
	)
	guardrail := llm.NewScripted(nil, testPricing())

	runner := newRunner(writer, guardrail, t)
	runner.Agent.MaxRetries = 2
	runner.Agent.MaxCostPerRetry = 0.0000001 // every writer call blows it
	runner.Agent.MaxCostPerLoop = 0          // unlimited, so the retry cap is what stops the loop

	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Reason != hint.ReasonMaxRetries {
		t.Fatalf("reason = %q, want %q — an unjudged hint must not count as stalled",
			got.Reason, hint.ReasonMaxRetries)
	}
	if guardrail.Remaining() != 0 {
		t.Errorf("guardrail responses left = %d, want the guardrail never consulted", guardrail.Remaining())
	}
}

func TestRun_GuardrailUnreadable(t *testing.T) {
	tests := []struct {
		label string
		resp  llm.ScriptedResponse
	}{
		{"prose instead of JSON", llm.ScriptedResponse{JSON: `Looks great to me!`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}},
		{"wrong schema", llm.ScriptedResponse{JSON: `{"looks_good": true}`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}},
		{"approved as a string, not a bool", llm.ScriptedResponse{JSON: `{"approved": "yes", "reason": "fine"}`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			writer := llm.NewScripted(nil, testPricing(),
				llm.ScriptedResponse{JSON: hintJSON("Which window never gets scored?"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
			)
			guardrail := llm.NewScripted(nil, testPricing(), tt.resp)

			runner := newRunner(writer, guardrail, t)
			got, err := runner.Run(context.Background(), baseParams())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Status != hint.StatusNoHint || got.Reason != hint.ReasonGuardrailFailed {
				t.Fatalf("status/reason = %q/%q, want %q/%q", got.Status, got.Reason, hint.StatusNoHint, hint.ReasonGuardrailFailed)
			}
			if len(got.Rejected) != 0 {
				t.Errorf("rejected = %+v, a fail-closed guardrail is not an ordinary rejection", got.Rejected)
			}
		})
	}
}

// The other half of TestRun_GuardrailUnreadable: an unreadable *answer* is
// a fail-closed verdict, but an interruption is not an answer at all. A dead
// connection or a cancelled context used to be reported as a completed
// no_hint/guardrail_failed, so the request terminated instead of staying
// reclaimable and the rest of the retry budget was thrown away. It has to
// propagate as infrastructure failure instead.
func TestRun_GuardrailTransportErrorIsInfraFailure(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("Which window never gets scored?"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
	)
	guardrail := llm.NewScripted(nil, testPricing(), llm.ScriptedResponse{Err: context.DeadlineExceeded})

	runner := newRunner(writer, guardrail, t)
	got, err := runner.Run(context.Background(), baseParams())
	if err == nil {
		t.Fatalf("Run: want an error, got status/reason %q/%q", got.Status, got.Reason)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap the transport failure", err)
	}
}

func TestRun_HealthyGuardrailStillApproves(t *testing.T) {
	// The inverse of TestRun_GuardrailUnreadable: a fail-closed check that
	// fails on everything would just be an outage, so also assert a
	// well-formed approval still gets through.
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("Which window never gets scored?"), Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}},
	)
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(true, "makes them think"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusApproved {
		t.Fatalf("status = %q, want %q", got.Status, hint.StatusApproved)
	}
}

// TestRun_MaxCostPerRetry_SkipsGuardrailButNextAttemptGetsAFreshBudget pins
// the per-retry cap's scope: it bounds one attempt's writer call, not the
// loop's running total. An expensive first attempt must skip its guardrail
// call (the last point at which spending can still be avoided) while a cheap
// second attempt still reaches the guardrail and can be approved.
func TestRun_MaxCostPerRetry_SkipsGuardrailButNextAttemptGetsAFreshBudget(t *testing.T) {
	writer := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: hintJSON("expensive attempt"), Usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000}},
		llm.ScriptedResponse{JSON: hintJSON("cheap attempt"), Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}},
	)
	// Exactly one scripted verdict: if the first attempt consulted the
	// guardrail, the second would find the script exhausted and panic.
	guardrail := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: verdictJSON(true, "makes them think"), Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)

	runner := newRunner(writer, guardrail, t)
	runner.Agent.MaxRetries = 3
	runner.Agent.MaxCostPerRetry = 0.001 // only the first attempt's writer call exceeds this

	got, err := runner.Run(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != hint.StatusApproved {
		t.Fatalf("status = %q, want %q (a fresh attempt must get a fresh per-retry budget)", got.Status, hint.StatusApproved)
	}
	if got.Hint != "cheap attempt" {
		t.Errorf("text = %q, want the second attempt's hint", got.Hint)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the capped attempt still counts as used)", got.Attempts)
	}
	if guardrail.Remaining() != 0 {
		t.Errorf("guardrail responses remaining = %d, want 0 (exactly one guardrail call)", guardrail.Remaining())
	}
	if writer.Remaining() != 0 {
		t.Errorf("writer responses remaining = %d, want 0", writer.Remaining())
	}
}
