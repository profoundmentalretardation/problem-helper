package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
)

// ScriptedResponse is one canned reply: either a JSON body plus usage, or an
// error to return in its place.
type ScriptedResponse struct {
	JSON  string
	Usage Usage
	Err   error
}

// Scripted is a deterministic fake ChatClient for testing agent loops
// without an API key or network access: it replays a fixed sequence of
// responses in order and panics if consulted beyond what was scripted, so
// an unexpected call fails the test loudly instead of silently returning a
// zero value (mirrors platform/mock's "unscripted call panics" pattern).
//
// If rec is non-nil, every consumed response is logged as its own
// llm_calls row via Cost/pricing, exactly like the real Client — so tests
// built against Scripted still see the accounting invariant hold.
type Scripted struct {
	rec       CallRecorder
	pricing   map[string]config.PricingConfig
	responses []ScriptedResponse
	calls     []Request
	next      int
}

// NewScripted builds a Scripted client that replays responses in order.
func NewScripted(rec CallRecorder, pricing map[string]config.PricingConfig, responses ...ScriptedResponse) *Scripted {
	return &Scripted{rec: rec, pricing: pricing, responses: responses}
}

// Chat returns the next scripted response, recording it like a real call.
func (s *Scripted) Chat(ctx context.Context, req Request) (Response, error) {
	if s.next >= len(s.responses) {
		panic(fmt.Sprintf("llm: scripted client consulted beyond its %d scripted response(s); unexpected call for agent %q, model %q", len(s.responses), req.Agent, req.Model))
	}
	r := s.responses[s.next]
	s.next++
	s.calls = append(s.calls, req)

	if r.Err != nil {
		return Response{}, r.Err
	}

	cost := Cost(r.Usage, s.pricing[req.Model])
	if s.rec != nil {
		if err := s.rec.InsertLLMCall(ctx, callRow(req, r.JSON, r.Usage, cost)); err != nil {
			return Response{}, fmt.Errorf("llm: recording scripted call: %w", err)
		}
	}

	return Response{JSON: json.RawMessage(r.JSON), Usage: r.Usage, Cost: cost}, nil
}

// Calls returns every request made so far, for test assertions.
func (s *Scripted) Calls() []Request { return s.calls }

// Remaining reports how many scripted responses were never consulted —
// tests should assert this is zero so a scripted response never silently
// goes unused.
func (s *Scripted) Remaining() int { return len(s.responses) - s.next }
