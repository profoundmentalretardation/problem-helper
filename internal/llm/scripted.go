package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

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
	// mu guards next/calls: a worker pool with Concurrency > 1 shares one
	// Scripted across pipelines, and without it the replay cursor races.
	mu    sync.Mutex
	calls []Request
	next  int
}

// NewScripted builds a Scripted client that replays responses in order.
func NewScripted(rec CallRecorder, pricing map[string]config.PricingConfig, responses ...ScriptedResponse) *Scripted {
	return &Scripted{rec: rec, pricing: pricing, responses: responses}
}

// Chat returns the next scripted response, recording it like a real call.
func (s *Scripted) Chat(ctx context.Context, req Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.next >= len(s.responses) {
		panic(fmt.Sprintf("llm: scripted client consulted beyond its %d scripted response(s); unexpected call for agent %q, model %q", len(s.responses), req.Agent, req.Model))
	}
	r := s.responses[s.next]
	s.next++
	s.calls = append(s.calls, req)

	cost := Cost(r.Usage, s.pricing[req.Model])

	if r.Err != nil {
		// A scripted failure is a call that happened, exactly like the real
		// Client's error path: the row carries the error text in place of a
		// response, so a test built on Scripted still sees the "every model
		// call writes an llm_calls row" invariant it is meant to model. It is
		// written on a context detached from ctx for the Client's reason too —
		// the failure being scripted is most often a cancellation, and
		// recording on the dying context would drop the row precisely on the
		// path worth recording.
		if s.rec != nil {
			recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
			defer cancel()
			if err := s.rec.InsertLLMCall(recCtx, callRow(req, "error: "+r.Err.Error(), r.Usage, cost)); err != nil {
				return Response{Usage: r.Usage, Cost: cost}, errors.Join(r.Err, fmt.Errorf("llm: recording scripted call: %w", err))
			}
		}
		// Usage rides along with the error, like the real Client's spent():
		// callers charge their cost caps from the returned Response on both
		// paths, so a Scripted that dropped it would hide a cap regression.
		return Response{Usage: r.Usage, Cost: cost}, r.Err
	}

	// Detached for the same reason the real Client's record is: the call has
	// "happened" by the time we get here, so a ctx canceled in the window
	// before the insert must not lose the row.
	if s.rec != nil {
		recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		if err := s.rec.InsertLLMCall(recCtx, callRow(req, r.JSON, r.Usage, cost)); err != nil {
			return Response{}, fmt.Errorf("llm: recording scripted call: %w", err)
		}
	}

	return Response{JSON: json.RawMessage(r.JSON), Usage: r.Usage, Cost: cost}, nil
}

// Calls returns every request made so far, for test assertions.
func (s *Scripted) Calls() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.calls...)
}

// Remaining reports how many scripted responses were never consulted —
// tests should assert this is zero so a scripted response never silently
// goes unused.
func (s *Scripted) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.responses) - s.next
}
