// metaloop.go is the nightly sweep that runs the curator
// (internal/agent/curator) across every user with unprocessed raw
// mistakes — decoupled from the request pipeline entirely, per the plan's
// "Nightly metaloop (curator)" section. It is also what
// POST /admin/metaloop/run drives on demand (see internal/api).
package worker

import (
	"context"
	"fmt"
	"log"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/curator"
)

// MistakeUserLister is the narrow store dependency Metaloop needs to build
// its worklist; *store.Store satisfies it.
type MistakeUserLister interface {
	ListUsersWithUnprocessedMistakes(ctx context.Context) ([]string, error)
}

// CuratorRunner runs the curator agent for one user's batch of unprocessed
// raw mistakes; *curator.Runner satisfies it.
type CuratorRunner interface {
	Run(ctx context.Context, userID string) (curator.Result, error)
}

// MetaloopSummary reports what one sweep did, returned by the admin trigger
// for operator visibility and used by the cron loop for logging.
type MetaloopSummary struct {
	UsersProcessed int
	Merged         int
	Created        int
	GaveUp         int
}

// Metaloop runs the curator for every user with unprocessed raw mistakes.
type Metaloop struct {
	Store   MistakeUserLister
	Curator CuratorRunner
	Logger  *log.Logger
}

// Run walks every user with unprocessed raw mistakes and runs the curator
// for each. A single user's curator error is logged and skipped rather than
// aborting the sweep — one bad batch must not block every other user's.
func (m *Metaloop) Run(ctx context.Context) (MetaloopSummary, error) {
	users, err := m.Store.ListUsersWithUnprocessedMistakes(ctx)
	if err != nil {
		return MetaloopSummary{}, fmt.Errorf("worker: listing users with unprocessed mistakes: %w", err)
	}

	var summary MetaloopSummary
	for _, userID := range users {
		result, err := m.Curator.Run(ctx, userID)
		if err != nil {
			m.logf("metaloop: curator for user %s: %v", userID, err)
			continue
		}
		summary.UsersProcessed++
		summary.Merged += result.Merged
		summary.Created += result.Created
		if result.Status == curator.StatusGaveUp {
			summary.GaveUp++
		}
	}
	return summary, nil
}

func (m *Metaloop) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
