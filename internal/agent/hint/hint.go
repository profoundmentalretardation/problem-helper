// Package hint runs loop 2: turn loop 1's verified fix into a hint the
// student can act on without being handed the answer. The hard part is
// rejecting hints, not writing them — cheap deterministic rules (rules.go)
// run first, so a hopeless hint never reaches a model; anything that
// survives goes to a guardrail model from a different family than the
// writer (guardrail.go), and only its explicit approval is a pass.
// Rejection reasons (from either source) are fed back to the writer as the
// next turn in the same conversation; proposing the same hint twice ends
// the loop rather than burning another retry on it.
package hint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
)

// Status is the outcome of a Run.
type Status string

const (
	StatusApproved Status = "approved"
	StatusNoHint   Status = "no_hint"
)

// Reason explains a StatusNoHint outcome.
type Reason string

const (
	ReasonMaxRetries      Reason = "max_retries"
	ReasonCostCap         Reason = "cost_cap"
	ReasonStalled         Reason = "stalled"          // same hint proposed twice
	ReasonGuardrailFailed Reason = "guardrail_failed" // unreadable reply or dead connection
)

// Rejected is one hint the loop tried and gave up on, kept for review.
type Rejected struct {
	Hint   string
	By     string // "rules" or "model"
	Reason string
}

// Params is one hint loop invocation.
type Params struct {
	RequestID uuid.UUID

	// OriginalCode is the student's cleaned (post-shield) code; WorkingCode
	// is loop 1's verified fix. The diff between them is what both agents
	// see — never the two full files.
	OriginalCode string
	WorkingCode  string
}

// Result is a Run outcome.
type Result struct {
	Status   Status
	Reason   Reason // set only when Status == StatusNoHint
	Hint     string // set only when Status == StatusApproved
	Rejected []Rejected
	Attempts int
}

// Runner runs the hint loop. Chat is the hint-writer agent; Guardrail is a
// second ChatClient expected to be configured with a different model
// family than Chat.
type Runner struct {
	Chat              llm.ChatClient
	Guardrail         llm.ChatClient
	Template          prompt.Template // prompts/hint.md
	GuardrailTemplate prompt.Template // prompts/guardrail.md
	Agent             config.AgentConfig
	GuardrailAgent    config.AgentConfig
}

type hintResponse struct {
	Hint string `json:"hint"`
}

var hintSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"hint": map[string]any{"type": "string"},
	},
	"required": []any{"hint"},
}

// Run executes the hint loop until a hint is approved, retries are
// exhausted, a cost cap is hit, the same hint is proposed twice, or the
// guardrail cannot be read at all.
func (r *Runner) Run(ctx context.Context, p Params) (Result, error) {
	diff := unifiedDiff(p.OriginalCode, p.WorkingCode)

	rendered, err := r.Template.Render(map[string]string{
		"diff":         diff,
		"working_code": p.WorkingCode,
	})
	if err != nil {
		return Result{}, fmt.Errorf("hint: rendering prompt: %w", err)
	}

	messages := []llm.Message{{Role: "system", Content: rendered}}
	seen := make(map[string]bool)
	var rejected []Rejected
	loopCost := 0.0
	attempts := 0

	for {
		if r.Agent.MaxCostPerLoop > 0 && loopCost >= r.Agent.MaxCostPerLoop {
			return Result{Status: StatusNoHint, Reason: ReasonCostCap, Rejected: rejected, Attempts: attempts}, nil
		}
		if attempts >= r.Agent.MaxRetries {
			return Result{Status: StatusNoHint, Reason: ReasonMaxRetries, Rejected: rejected, Attempts: attempts}, nil
		}
		attempts++

		resp, err := r.Chat.Chat(ctx, llm.Request{
			RequestID:       p.RequestID,
			Agent:           "hint",
			Model:           r.Agent.Model,
			Temperature:     r.Agent.Temperature,
			ReasoningEffort: r.Agent.ReasoningEffort,
			Attempt:         attempts,
			Messages:        messages,
			SchemaName:      "hint_text",
			Schema:          hintSchema,
		})
		if err != nil {
			// Chat populates Usage/Cost on its error returns, so the loop is
			// charged for what a failed call really spent. A model that
			// cannot produce schema-shaped JSON even after Chat's own retry
			// burns this attempt rather than failing the whole request:
			// "we declined to hint" (no_hint) and "our infra broke" (failed)
			// are the split the pipeline exists to keep.
			loopCost += parseCost(resp.Cost)
			if errors.Is(err, llm.ErrInvalidResponse) {
				continue
			}
			return Result{}, fmt.Errorf("hint: chat: %w", err)
		}
		// retryCost is this attempt's writer call only, unlike loopCost which
		// bounds the whole loop. The cap is checked here, before the
		// guardrail call, because that is the last point at which spending
		// can still be avoided — so the guardrail's own cost is never part of
		// the amount compared against it (it lands in loopCost at :167).
		retryCost := parseCost(resp.Cost)
		loopCost += retryCost

		messages = append(messages, llm.Message{Role: "assistant", Content: string(resp.JSON)})

		// llm.validateJSON only checks that the required keys are *present*,
		// never their types, so {"hint": 42} reaches here having satisfied
		// Chat. That is a model formatting mistake, exactly like the
		// ErrInvalidResponse case above — burn the attempt and feed the error
		// back, rather than erroring the request into status=failed and
		// reporting an internal fault for a request the pipeline handled as
		// designed.
		var out hintResponse
		if err := json.Unmarshal(resp.JSON, &out); err != nil {
			messages = append(messages, llm.Message{
				Role: "user", Content: `{"approved":false,"reason":"your reply was not a JSON object with a string \"hint\" field"}`,
			})
			continue
		}
		hintText := out.Hint

		if r.Agent.MaxCostPerRetry > 0 && retryCost >= r.Agent.MaxCostPerRetry {
			// This attempt has already spent its budget; skip the guardrail
			// call and let the next attempt start with a fresh one. Same
			// enforcement point as the repair loop's per-retry cap.
			//
			// Checked before `seen` is recorded, and followed by a user turn:
			// this hint was never judged, so banning it would make the next
			// attempt's identical (and still unjudged) reply terminate the
			// loop as ReasonStalled, and without a turn here attempt N+1 would
			// resume a conversation whose last message is the model's own
			// reply — no feedback at all, so it simply repeats itself.
			messages = append(messages, llm.Message{
				Role: "user", Content: `{"approved":false,"reason":"that attempt exceeded its cost budget before it could be reviewed; answer more concisely"}`,
			})
			continue
		}

		if seen[hintText] {
			return Result{Status: StatusNoHint, Reason: ReasonStalled, Rejected: rejected, Attempts: attempts}, nil
		}
		seen[hintText] = true

		v, err := r.judge(ctx, p.RequestID, attempts, diff, p.WorkingCode, hintText, p.OriginalCode)
		if err != nil {
			return Result{}, err
		}
		loopCost += v.Cost

		if !v.OK {
			return Result{Status: StatusNoHint, Reason: ReasonGuardrailFailed, Rejected: rejected, Attempts: attempts}, nil
		}
		if v.Approved {
			return Result{Status: StatusApproved, Hint: hintText, Rejected: rejected, Attempts: attempts}, nil
		}

		rejected = append(rejected, Rejected{Hint: hintText, By: v.By, Reason: v.Reason})
		reply, err := json.Marshal(struct {
			Approved bool   `json:"approved"`
			Reason   string `json:"reason"`
		}{false, v.Reason})
		if err != nil {
			return Result{}, fmt.Errorf("hint: encoding rejection reply: %w", err)
		}
		messages = append(messages, llm.Message{Role: "user", Content: string(reply)})
	}
}

// parseCost parses a cost string as produced by llm.Response.Cost; an
// unparseable value (should not happen) counts as zero rather than erroring
// the whole loop over an accounting detail.
func parseCost(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// unifiedDiff renders a unified diff of before -> after, the only view of
// the change either agent gets (never the two full files together).
func unifiedDiff(before, after string) string {
	if before == after {
		return ""
	}
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: "original",
		ToFile:   "working",
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return ""
	}
	return out
}
