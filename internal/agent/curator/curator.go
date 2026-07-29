// Package curator runs the nightly metaloop: fold one user's unprocessed
// raw_mistakes observations (written by the repair loop, see
// internal/agent/repair) into their per-user mistakes tally. It runs after
// the student already has their hint — nothing it does is on the request's
// critical path — and it is the only place the system decides whether a new
// observation is the same underlying habit as one already on file, or
// something genuinely new.
//
// Like repair, the model has no native tool-calling channel here —
// llm.ChatClient only exchanges schema-validated JSON — so the three tools
// the plan describes (merge_into, create_mistake, finish) are emulated with
// a discriminated "action" field on a single response schema.
package curator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// Status is the outcome of a Run.
type Status string

const (
	// StatusNoUnprocessed means the user had no unprocessed raw mistakes;
	// zero model calls were made.
	StatusNoUnprocessed Status = "no_unprocessed"
	// StatusCurated means finish was called after at least one merge_into
	// or create_mistake; the batch's raw mistakes are marked processed.
	StatusCurated Status = "curated"
	// StatusNothingToSave means finish was called without saving
	// anything — a normal outcome; the batch is still marked processed.
	StatusNothingToSave Status = "nothing_to_save"
	// StatusGaveUp means the call budget was exhausted before finish was
	// ever called (malformed replies, or a model that never wraps up).
	// Nothing is written and the batch's raw mistakes stay unprocessed,
	// so they are retried on the next sweep.
	StatusGaveUp Status = "gave_up"
)

// RawMistakeStore is the raw_mistakes dependency Run needs; *store.Store
// satisfies it.
type RawMistakeStore interface {
	ListUnprocessedRawMistakes(ctx context.Context, userID string) ([]store.RawMistake, error)
	MarkRawMistakesProcessed(ctx context.Context, ids []uuid.UUID) error
}

// MistakeStore is the mistakes dependency Run needs; *store.Store satisfies
// it.
type MistakeStore interface {
	ListMistakes(ctx context.Context, userID string) ([]store.Mistake, error)
	CreateMistake(ctx context.Context, m store.Mistake) error
	MergeMistake(ctx context.Context, userID string, id uuid.UUID) error
}

// Result is a Run outcome.
type Result struct {
	Status  Status
	Merged  int
	Created int
	Calls   int
}

// Runner runs the curator for one user's batch of unprocessed raw mistakes.
type Runner struct {
	Chat        llm.ChatClient
	RawMistakes RawMistakeStore
	Mistakes    MistakeStore
	Template    prompt.Template // prompts/curator.md
	Agent       config.AgentConfig
}

// curatorAction is the single schema every curator Chat call uses: a
// discriminated union over the three tools the model can invoke.
type curatorAction struct {
	Action      string `json:"action"`
	MistakeID   string `json:"mistake_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

var responseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"action": map[string]any{
			"type": "string",
			"enum": []any{"merge_into", "create_mistake", "finish"},
		},
		"mistake_id":  map[string]any{"type": "string"},
		"title":       map[string]any{"type": "string"},
		"description": map[string]any{"type": "string"},
	},
	"required": []any{"action"},
}

// Run folds userID's unprocessed raw mistakes into their mistakes tally.
// The call budget is one call per raw mistake in the batch (each may need
// its own merge_into or create_mistake) plus Agent.MaxRetries of slack for
// the closing finish and any malformed replies. Sizing it from MaxRetries
// alone would make any batch needing more actions than that permanently
// unfinishable: finish is never reached, the batch is never marked
// processed, and every later sweep re-sends an ever-growing batch.
// Exhausting the budget leaves the batch unprocessed (retried next sweep)
// rather than looping forever.
func (r *Runner) Run(ctx context.Context, userID string) (Result, error) {
	raw, err := r.RawMistakes.ListUnprocessedRawMistakes(ctx, userID)
	if err != nil {
		return Result{}, fmt.Errorf("curator: listing unprocessed raw mistakes: %w", err)
	}
	if len(raw) == 0 {
		return Result{Status: StatusNoUnprocessed}, nil
	}

	existing, err := r.Mistakes.ListMistakes(ctx, userID)
	if err != nil {
		return Result{}, fmt.Errorf("curator: listing existing mistakes: %w", err)
	}

	rendered, err := r.Template.Render(map[string]string{
		"raw_mistakes":      renderRawMistakes(raw),
		"existing_mistakes": renderMistakes(existing),
	})
	if err != nil {
		return Result{}, fmt.Errorf("curator: rendering prompt: %w", err)
	}

	// Curator runs are per-user, decoupled from any single request, but
	// llm_calls.request_id is NOT NULL — attribute the cost to the request
	// that produced the oldest mistake in this batch, a request that is
	// guaranteed to exist.
	requestID := raw[0].RequestID

	messages := []llm.Message{{Role: "system", Content: rendered}}
	var merged, created, calls int
	callBudget := len(raw) + r.Agent.MaxRetries

	for {
		if calls >= callBudget {
			return Result{Status: StatusGaveUp, Merged: merged, Created: created, Calls: calls}, nil
		}
		calls++

		resp, err := r.Chat.Chat(ctx, llm.Request{
			RequestID:       requestID,
			Agent:           "curator",
			Model:           r.Agent.Model,
			Temperature:     r.Agent.Temperature,
			ReasoningEffort: r.Agent.ReasoningEffort,
			Attempt:         calls,
			Messages:        messages,
			SchemaName:      "curator_action",
			Schema:          responseSchema,
		})
		if err != nil {
			return Result{}, fmt.Errorf("curator: chat: %w", err)
		}

		var action curatorAction
		if jsonErr := json.Unmarshal(resp.JSON, &action); jsonErr != nil {
			messages = append(messages,
				llm.Message{Role: "assistant", Content: string(resp.JSON)},
				llm.Message{Role: "user", Content: `{"ok":false,"error":"could not parse your reply as JSON"}`},
			)
			continue
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: string(resp.JSON)})

		switch action.Action {
		case "merge_into":
			id, parseErr := uuid.Parse(action.MistakeID)
			if parseErr != nil {
				messages = append(messages, llm.Message{Role: "user", Content: `{"ok":false,"error":"mistake_id must be a valid uuid"}`})
				continue
			}
			if err := r.Mistakes.MergeMistake(ctx, userID, id); err != nil {
				// A hallucinated (or another user's) id is the model's
				// mistake, not an infrastructure failure — feed it back and
				// let it retry, the same as an unparseable id. Failing the
				// whole run here would strand the batch forever, since the
				// next sweep re-sends the same prompt.
				if errors.Is(err, store.ErrUnknownMistake) {
					messages = append(messages, llm.Message{Role: "user", Content: `{"ok":false,"error":"no mistake with that id"}`})
					continue
				}
				return Result{}, fmt.Errorf("curator: merging mistake: %w", err)
			}
			merged++
			messages = append(messages, llm.Message{Role: "user", Content: `{"ok":true}`})

		case "create_mistake":
			if action.Title == "" || action.Description == "" {
				messages = append(messages, llm.Message{Role: "user", Content: `{"ok":false,"error":"create_mistake requires title and description"}`})
				continue
			}
			if err := r.Mistakes.CreateMistake(ctx, store.Mistake{
				UserID:      userID,
				Title:       action.Title,
				Description: action.Description,
			}); err != nil {
				return Result{}, fmt.Errorf("curator: creating mistake: %w", err)
			}
			created++
			messages = append(messages, llm.Message{Role: "user", Content: `{"ok":true}`})

		case "finish":
			ids := make([]uuid.UUID, len(raw))
			for i, m := range raw {
				ids[i] = m.ID
			}
			if err := r.RawMistakes.MarkRawMistakesProcessed(ctx, ids); err != nil {
				return Result{}, fmt.Errorf("curator: marking raw mistakes processed: %w", err)
			}
			status := StatusCurated
			if merged == 0 && created == 0 {
				status = StatusNothingToSave
			}
			return Result{Status: status, Merged: merged, Created: created, Calls: calls}, nil

		default:
			messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf(`{"ok":false,"error":"no tool called %q"}`, action.Action)})
		}
	}
}

func renderRawMistakes(raw []store.RawMistake) string {
	lines := make([]string, len(raw))
	for i, m := range raw {
		lines[i] = fmt.Sprintf("- %s", m.Text)
	}
	return strings.Join(lines, "\n")
}

func renderMistakes(mistakes []store.Mistake) string {
	lines := make([]string, len(mistakes))
	for i, m := range mistakes {
		lines[i] = fmt.Sprintf("- id=%s (%dx) %s: %s", m.ID, m.Count, m.Title, m.Description)
	}
	return strings.Join(lines, "\n")
}
