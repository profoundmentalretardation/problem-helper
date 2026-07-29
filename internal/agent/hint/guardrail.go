package hint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/llm"
)

var guardrailSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"approved": map[string]any{"type": "boolean"},
		"reason":   map[string]any{"type": "string"},
	},
	"required": []any{"approved", "reason"},
}

// verdict is the outcome of judging one candidate hint, either by the
// deterministic rules or by the guardrail model. OK=false means the
// guardrail could not be read at all (prose, wrong shape, dead connection)
// — that is never an approval, and it stops the loop rather than being fed
// back as ordinary criticism.
type verdict struct {
	OK       bool
	By       string // "rules" or "model"
	Approved bool
	Reason   string
	Cost     float64
}

// judge checks hintText against the deterministic rules first — a leak they
// catch never reaches the guardrail model, so it costs nothing. Only a hint
// that survives the rules is judgement-call territory for the guardrail.
func (r *Runner) judge(ctx context.Context, requestID uuid.UUID, attempt int, diff, workingCode, hintText, original string) (verdict, error) {
	if reasons := looksExplicit(hintText, original, workingCode); len(reasons) > 0 {
		return verdict{OK: true, By: "rules", Approved: false, Reason: reasons[0]}, nil
	}
	return r.checkGuardrail(ctx, requestID, attempt, diff, workingCode, hintText)
}

// checkGuardrail renders the guardrail prompt and asks the guardrail model
// to approve or reject hintText. A malformed or unreadable reply (prose,
// the wrong JSON shape, a dead connection) reports OK=false — it is never
// silently treated as approval.
func (r *Runner) checkGuardrail(ctx context.Context, requestID uuid.UUID, attempt int, diff, workingCode, hintText string) (verdict, error) {
	rendered, err := r.GuardrailTemplate.Render(map[string]string{
		"diff":         diff,
		"working_code": workingCode,
		"hint":         hintText,
	})
	if err != nil {
		return verdict{}, fmt.Errorf("hint: rendering guardrail prompt: %w", err)
	}

	resp, err := r.Guardrail.Chat(ctx, llm.Request{
		RequestID:       requestID,
		Agent:           "guardrail",
		Model:           r.GuardrailAgent.Model,
		Temperature:     r.GuardrailAgent.Temperature,
		ReasoningEffort: r.GuardrailAgent.ReasoningEffort,
		Attempt:         attempt,
		Messages:        []llm.Message{{Role: "system", Content: rendered}},
		SchemaName:      "guardrail_verdict",
		Schema:          guardrailSchema,
	})
	if err != nil {
		// An unreadable *answer* is OK=false — never approval. An
		// interruption is not an answer at all: a cancelled context (a
		// worker shutting down) or a transport failure used to be reported
		// as a completed no_hint/guardrail_failed outcome, so the request
		// terminated instead of staying reclaimable and the remaining
		// retry budget was thrown away. Only llm.ErrInvalidResponse — the
		// model genuinely failed to produce the schema — is a verdict.
		// Either way the partial spend is carried, as on every other path.
		if !errors.Is(err, llm.ErrInvalidResponse) {
			return verdict{Cost: parseCost(resp.Cost)}, fmt.Errorf("hint: guardrail call: %w", err)
		}
		return verdict{OK: false, By: "model", Cost: parseCost(resp.Cost)}, nil
	}

	// Parsed as a generic map, not a struct: a struct decode would silently
	// default a missing or mistyped "approved" field to false, which reads
	// as an ordinary rejection instead of the "wrong schema is never
	// approval" case this guards against.
	var raw map[string]any
	if jsonErr := json.Unmarshal(resp.JSON, &raw); jsonErr != nil {
		return verdict{OK: false, By: "model", Cost: parseCost(resp.Cost)}, nil
	}
	approved, ok := raw["approved"].(bool)
	if !ok {
		return verdict{OK: false, By: "model", Cost: parseCost(resp.Cost)}, nil
	}
	reason, _ := raw["reason"].(string)
	return verdict{OK: true, By: "model", Approved: approved, Reason: reason, Cost: parseCost(resp.Cost)}, nil
}
