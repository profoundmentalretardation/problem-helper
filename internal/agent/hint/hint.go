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
			return Result{}, fmt.Errorf("hint: chat: %w", err)
		}
		loopCost += parseCost(resp.Cost)

		var out hintResponse
		if err := json.Unmarshal(resp.JSON, &out); err != nil {
			return Result{}, fmt.Errorf("hint: unmarshaling hint response: %w", err)
		}
		hintText := out.Hint
		messages = append(messages, llm.Message{Role: "assistant", Content: string(resp.JSON)})

		if seen[hintText] {
			return Result{Status: StatusNoHint, Reason: ReasonStalled, Rejected: rejected, Attempts: attempts}, nil
		}
		seen[hintText] = true

		if r.Agent.MaxCostPerRetry > 0 && loopCost >= r.Agent.MaxCostPerRetry {
			continue
		}

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
