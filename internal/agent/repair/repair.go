// Package repair runs loop 1: given a student's cleaned best-failing
// submission, ask a model to diagnose and fix it, verify the fix by running
// it on the platform as a system user, and retry until the fix is verified,
// retries are exhausted, or a cost cap is hit.
//
// The model has no native tool-calling channel here — llm.ChatClient only
// exchanges schema-validated JSON — so the "tools" described in
// prompts/repair.md (list this run's test results, inspect one test) are
// emulated with a single response schema carrying a discriminated "action"
// field ("list_test_results" | "get_test" | "submit"). Each non-submit
// action appends a plain assistant/user turn to the conversation and loops;
// "submit" ends the tool sub-loop for the current attempt and the proposed
// code is verified against the real platform.
//
// "The current run" a tool call reads from starts out as the run that
// produced the student's own best submission (Params.BaselineRunID, e.g.
// the platform's submission id — already judged, so its per-test results
// are queryable) and becomes the most recent failed attempt's run once one
// exists. The model never chooses a run id, only a test_id.
package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// Status is the outcome of a Run.
type Status string

const (
	StatusFixed Status = "fixed"
	StatusNoFix Status = "no_fix"
)

// Reason explains a StatusNoFix outcome.
type Reason string

const (
	ReasonMaxRetries Reason = "max_retries"
	ReasonCostCap    Reason = "cost_cap"
)

// passVerdict is the platform.TestCase.Verdict value a test reports when it
// passed. Everything else counts as failing, including the degraded
// {index, verdict} shape a platform with hidden tests may return.
const passVerdict = "OK"

// defaultPollInterval paces RunResult polling when a verification run isn't
// done yet. Tests script Done=true on the first poll, so it's never slept.
const defaultPollInterval = 10 * time.Millisecond

// maxToolCallsPerAttempt backstops an unbounded tool loop when
// max_cost_per_retry is 0 (unlimited) and a model never calls submit.
const maxToolCallsPerAttempt = 20

// Mistake is one habit the model flagged as worth remembering about the
// student, alongside a proposed fix — independent of whether that
// particular fix went on to pass verification.
type Mistake struct {
	Text string
}

// Params is one repair loop invocation.
type Params struct {
	RequestID uuid.UUID
	UserID    string
	ProblemID string
	Language  string

	ProblemStatement string
	UserCode         string   // cleaned (post-shield) code
	Mistakes         []string // this student's top-N recurring mistakes, rendered lines

	// BaselineRunID is the already-judged run the student's best submission
	// produced, e.g. its platform submission id — the starting point for
	// "the current run" the tools read from.
	BaselineRunID      string
	BaselineTestsTotal int

	// PollInterval overrides defaultPollInterval; zero uses the default.
	PollInterval time.Duration
}

// Result is a Run outcome.
type Result struct {
	Status   Status
	Reason   Reason // set only when Status == StatusNoFix
	Code     string
	Mistakes []Mistake
	RunID    string // the verified run's id, set only when Status == StatusFixed
	Attempts int
}

// EventRecorder persists one events row; *store.Store satisfies it. Used to
// record a verification run's id before polling starts, so a crash-recovered
// worker resumes the existing run instead of resubmitting.
type EventRecorder interface {
	AppendEvent(ctx context.Context, requestID uuid.UUID, kind string, payload []byte) error
}

// MistakeRecorder persists one raw_mistakes row; *store.Store satisfies it.
type MistakeRecorder interface {
	InsertRawMistake(ctx context.Context, m store.RawMistake) error
}

// Runner runs the repair loop. Events and Mistakes are optional (nil skips
// recording) so tests can exercise the loop without either.
type Runner struct {
	Chat     llm.ChatClient
	Platform platform.Platform
	Template prompt.Template
	Agent    config.AgentConfig
	Events   EventRecorder
	Mistakes MistakeRecorder
}

// modelAction is the single schema every repair Chat call uses: a
// discriminated union over the three tools the model can invoke.
type modelAction struct {
	Action   string `json:"action"`
	TestID   *int   `json:"test_id,omitempty"`
	Code     string `json:"code,omitempty"`
	Mistakes []struct {
		Text string `json:"text"`
	} `json:"mistakes,omitempty"`
}

var responseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"action": map[string]any{
			"type": "string",
			"enum": []any{"list_test_results", "get_test", "submit"},
		},
		"test_id": map[string]any{"type": "integer"},
		"code":    map[string]any{"type": "string"},
		"mistakes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []any{"text"},
			},
		},
	},
	"required": []any{"action"},
}

// Run executes the repair loop until a fix verifies, retries are exhausted,
// or a cost cap is hit. Every attempt re-renders the system prompt with the
// previous attempt's code (or "none" for the first); within an attempt, the
// tool sub-loop shares one conversation.
func (r *Runner) Run(ctx context.Context, p Params) (Result, error) {
	pollInterval := p.PollInterval
	if pollInterval == 0 {
		pollInterval = defaultPollInterval
	}

	baseline, err := r.fetchTestCases(ctx, p.BaselineRunID, p.BaselineTestsTotal)
	if err != nil {
		return Result{}, fmt.Errorf("repair: fetching baseline test results: %w", err)
	}

	currentRunID := p.BaselineRunID
	currentTestsTotal := p.BaselineTestsTotal
	previousCode := ""
	loopCost := 0.0
	attempts := 0

	for {
		if r.Agent.MaxCostPerLoop > 0 && loopCost >= r.Agent.MaxCostPerLoop {
			return Result{Status: StatusNoFix, Reason: ReasonCostCap, Attempts: attempts}, nil
		}
		if attempts >= r.Agent.MaxRetries {
			return Result{Status: StatusNoFix, Reason: ReasonMaxRetries, Attempts: attempts}, nil
		}
		attempts++

		proposed, mistakes, cost, err := r.runAttempt(ctx, p, attempts, previousCode, currentRunID, currentTestsTotal)
		loopCost += cost
		if err != nil {
			return Result{}, err
		}
		if proposed == nil {
			// Attempt aborted (per-retry cost cap or the step backstop)
			// before ever proposing code; still counts as a used retry.
			continue
		}

		if err := r.recordMistakes(ctx, p, mistakes); err != nil {
			return Result{}, err
		}

		run, err := r.Platform.SubmitAsSystem(ctx, p.ProblemID, proposed.Code, p.Language)
		if err != nil {
			return Result{}, fmt.Errorf("repair: submitting for verification: %w", err)
		}
		if err := r.recordRunID(ctx, p.RequestID, run.ID, attempts); err != nil {
			return Result{}, err
		}

		result, err := r.pollUntilDone(ctx, run, pollInterval)
		if err != nil {
			return Result{}, err
		}

		current, err := r.fetchTestCases(ctx, run.ID, result.TestsTotal)
		if err != nil {
			return Result{}, fmt.Errorf("repair: fetching verification test results: %w", err)
		}

		if success(baseline, current) {
			return Result{
				Status: StatusFixed, Code: proposed.Code, Mistakes: mistakes,
				RunID: run.ID, Attempts: attempts,
			}, nil
		}

		currentRunID = run.ID
		currentTestsTotal = result.TestsTotal
		previousCode = proposed.Code
	}
}

// runAttempt runs the tool sub-loop for one attempt: repeated Chat calls
// until the model submits code, the per-retry cost cap is hit, or the step
// backstop is reached. Returns a nil proposal (not an error) when the
// attempt was aborted without a proposal.
func (r *Runner) runAttempt(
	ctx context.Context, p Params, attempt int, previousCode, currentRunID string, currentTestsTotal int,
) (*modelAction, []Mistake, float64, error) {
	rendered, err := r.Template.Render(map[string]string{
		"problem_statement": p.ProblemStatement,
		"user_code":         p.UserCode,
		"mistakes":          strings.Join(p.Mistakes, "\n"),
		"previous_code":     previousCode,
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("repair: rendering prompt: %w", err)
	}

	messages := []llm.Message{{Role: "system", Content: rendered}}
	retryCost := 0.0

	for calls := 0; calls < maxToolCallsPerAttempt; calls++ {
		if r.Agent.MaxCostPerRetry > 0 && retryCost >= r.Agent.MaxCostPerRetry {
			return nil, nil, retryCost, nil
		}

		resp, err := r.Chat.Chat(ctx, llm.Request{
			RequestID:       p.RequestID,
			Agent:           "repair",
			Model:           r.Agent.Model,
			Temperature:     r.Agent.Temperature,
			ReasoningEffort: r.Agent.ReasoningEffort,
			Attempt:         attempt,
			Messages:        messages,
			SchemaName:      "repair_action",
			Schema:          responseSchema,
		})
		if err != nil {
			return nil, nil, retryCost, fmt.Errorf("repair: chat: %w", err)
		}
		if cost, perr := strconv.ParseFloat(resp.Cost, 64); perr == nil {
			retryCost += cost
		}

		var action modelAction
		if err := json.Unmarshal(resp.JSON, &action); err != nil {
			messages = append(messages,
				llm.Message{Role: "assistant", Content: string(resp.JSON)},
				llm.Message{Role: "user", Content: `{"ok":false,"error":"could not parse your reply as JSON"}`},
			)
			continue
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: string(resp.JSON)})

		reply, done, err := r.handleAction(ctx, action, currentRunID, currentTestsTotal)
		if err != nil {
			return nil, nil, retryCost, err
		}
		if done {
			mistakes := make([]Mistake, 0, len(action.Mistakes))
			for _, m := range action.Mistakes {
				mistakes = append(mistakes, Mistake{Text: m.Text})
			}
			return &action, mistakes, retryCost, nil
		}
		messages = append(messages, llm.Message{Role: "user", Content: reply})
	}

	return nil, nil, retryCost, nil
}

// handleAction executes one non-submit tool call, or reports done=true for
// a valid submit. reply is the tool-result JSON to append as the next user
// turn (empty when done).
func (r *Runner) handleAction(ctx context.Context, action modelAction, currentRunID string, currentTestsTotal int) (reply string, done bool, err error) {
	switch action.Action {
	case "list_test_results":
		payload, err := r.listTestResults(ctx, currentRunID, currentTestsTotal)
		if err != nil {
			return "", false, fmt.Errorf("repair: listing test results: %w", err)
		}
		return payload, false, nil

	case "get_test":
		if action.TestID == nil || *action.TestID < 1 || *action.TestID > currentTestsTotal {
			return `{"ok":false,"error":"test_id out of range"}`, false, nil
		}
		tc, err := r.Platform.TestResult(ctx, currentRunID, *action.TestID)
		if err != nil {
			return "", false, fmt.Errorf("repair: fetching test result: %w", err)
		}
		out, _ := json.Marshal(struct {
			OK   bool              `json:"ok"`
			Test platform.TestCase `json:"test"`
		}{true, tc})
		return string(out), false, nil

	case "submit":
		if action.Code == "" {
			return `{"ok":false,"error":"submit requires non-empty code"}`, false, nil
		}
		return "", true, nil

	default:
		return fmt.Sprintf(`{"ok":false,"error":"no tool called %q"}`, action.Action), false, nil
	}
}

type testSummary struct {
	Index   int    `json:"index"`
	Verdict string `json:"verdict"`
}

// listTestResults returns up to Agent.NTestsShown test summaries for runID
// (0 or unset NTestsShown means show every test).
func (r *Runner) listTestResults(ctx context.Context, runID string, total int) (string, error) {
	limit := total
	if r.Agent.NTestsShown > 0 && r.Agent.NTestsShown < limit {
		limit = r.Agent.NTestsShown
	}

	tests := make([]testSummary, 0, limit)
	for i := 1; i <= limit; i++ {
		tc, err := r.Platform.TestResult(ctx, runID, i)
		if err != nil {
			return "", err
		}
		tests = append(tests, testSummary{Index: tc.Index, Verdict: tc.Verdict})
	}

	out, err := json.Marshal(struct {
		OK    bool          `json:"ok"`
		Total int           `json:"total"`
		Tests []testSummary `json:"tests"`
	}{true, total, tests})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// fetchTestCases fetches every test (1..total) for a run.
func (r *Runner) fetchTestCases(ctx context.Context, runID string, total int) (map[int]platform.TestCase, error) {
	out := make(map[int]platform.TestCase, total)
	for i := 1; i <= total; i++ {
		tc, err := r.Platform.TestResult(ctx, runID, i)
		if err != nil {
			return nil, err
		}
		out[i] = tc
	}
	return out, nil
}

// success reports whether every test the baseline run covers passes in
// current. Because that includes tests the baseline already passed, a
// verification run that "fixes" a previously-failing test while regressing
// a previously-passing one still fails this check — it's caught by the
// regressed test's own verdict, not by comparing pass counts.
func success(baseline, current map[int]platform.TestCase) bool {
	if len(baseline) == 0 {
		return false
	}
	for id := range baseline {
		c, ok := current[id]
		if !ok || c.Verdict != passVerdict {
			return false
		}
	}
	return true
}

func (r *Runner) pollUntilDone(ctx context.Context, run platform.RunResult, pollInterval time.Duration) (platform.RunResult, error) {
	result := run
	for !result.Done {
		time.Sleep(pollInterval)
		next, err := r.Platform.RunResult(ctx, run.ID)
		if err != nil {
			return platform.RunResult{}, fmt.Errorf("repair: polling run result: %w", err)
		}
		result = next
	}
	return result, nil
}

func (r *Runner) recordRunID(ctx context.Context, requestID uuid.UUID, runID string, attempt int) error {
	if r.Events == nil {
		return nil
	}
	payload, err := json.Marshal(struct {
		RunID   string `json:"run_id"`
		Attempt int    `json:"attempt"`
	}{runID, attempt})
	if err != nil {
		return fmt.Errorf("repair: encoding run-id event: %w", err)
	}
	if err := r.Events.AppendEvent(ctx, requestID, "repair_run_submitted", payload); err != nil {
		return fmt.Errorf("repair: recording run id: %w", err)
	}
	return nil
}

func (r *Runner) recordMistakes(ctx context.Context, p Params, mistakes []Mistake) error {
	if r.Mistakes == nil {
		return nil
	}
	for _, m := range mistakes {
		if err := r.Mistakes.InsertRawMistake(ctx, store.RawMistake{
			RequestID: p.RequestID,
			UserID:    p.UserID,
			Text:      m.Text,
		}); err != nil {
			return fmt.Errorf("repair: recording raw mistake: %w", err)
		}
	}
	return nil
}
