// worker.go is the queue side of the worker: claiming a pending
// help_requests row (FOR UPDATE SKIP LOCKED, so concurrent workers never
// double-claim), heartbeating it while RunPipeline is in flight, and
// periodically reclaiming rows whose heartbeat has gone stale (a crashed
// claimant) back to pending so another worker can resume them — resume_step
// is left untouched by a reclaim, so RunPipeline picks up where the last
// checkpoint left off (see pipeline.go).
package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

const (
	defaultConcurrency       = 1
	defaultPollInterval      = time.Second
	defaultHeartbeatInterval = 10 * time.Second
	defaultStaleAfter        = time.Minute
	defaultReclaimInterval   = 30 * time.Second
	defaultMetaloopInterval  = 24 * time.Hour
)

// QueueStore is the persistence dependency Worker needs to run the queue:
// claiming, heartbeating, and reclaiming stale rows; *store.Store satisfies
// it.
type QueueStore interface {
	ClaimNext(ctx context.Context, workerID string) (*store.HelpRequest, error)
	Heartbeat(ctx context.Context, id uuid.UUID, workerID string) (bool, error)
	ReclaimStale(ctx context.Context, staleAfter time.Duration) ([]uuid.UUID, error)
	GetHelpRequest(ctx context.Context, id uuid.UUID) (*store.HelpRequest, error)
}

// PipelineRunner runs one claimed request through the full pipeline;
// *Pipeline satisfies it. A narrow interface so tests can drive Worker
// without a real repair/hint/platform stack.
type PipelineRunner interface {
	RunPipeline(ctx context.Context, requestID uuid.UUID) error
}

// MetaloopRunner runs the nightly curator sweep; *Metaloop satisfies it. A
// separate interface (rather than depending on *Metaloop directly) keeps
// Worker's cron tests independent of the curator/store stack.
type MetaloopRunner interface {
	Run(ctx context.Context) (MetaloopSummary, error)
}

// Worker claims and runs help_requests rows until its context is canceled.
// The zero value is not usable; ID, Store and Pipeline are required, every
// other field falls back to a package default when zero.
type Worker struct {
	ID       string
	Store    QueueStore
	Pipeline PipelineRunner

	// Metaloop runs the nightly curator sweep; nil skips it entirely (no
	// cron goroutine is started).
	Metaloop MetaloopRunner

	// Concurrency is how many claim loops run at once; defaults to 1.
	Concurrency int
	// PollInterval paces an idle claim loop between empty claim attempts.
	PollInterval time.Duration
	// HeartbeatInterval paces how often an in-flight claim's heartbeat is
	// refreshed.
	HeartbeatInterval time.Duration
	// StaleAfter is how old a running row's heartbeat must be before it's
	// reclaimed.
	StaleAfter time.Duration
	// ReclaimInterval paces the periodic reclaim sweep; 0 uses the default,
	// a negative value disables the periodic sweep entirely (the startup
	// sweep in Run still happens once either way).
	ReclaimInterval time.Duration
	// MetaloopInterval paces the periodic curator sweep; 0 uses the
	// default (24h), a negative value disables it.
	MetaloopInterval time.Duration

	// Logger receives non-fatal operational messages (claim/heartbeat/
	// reclaim errors); defaults to log.Default().
	Logger *log.Logger
}

// Run claims and runs requests until ctx is canceled. On cancellation, Run
// stops claiming new work but waits for any already-claimed request to
// finish its pipeline run (heartbeating throughout, since a stale-looking
// heartbeat mid-drain would invite another worker to reclaim it) before
// returning. Run always returns nil; per-request failures are recorded on
// the request row and logged, never fatal to the pool.
func (w *Worker) Run(ctx context.Context) error {
	if reclaimed, err := w.reclaimSweep(ctx); err != nil {
		w.logf("startup reclaim: %v", err)
	} else if len(reclaimed) > 0 {
		w.logf("reclaimed %d stale request(s) at startup", len(reclaimed))
	}

	var wg sync.WaitGroup

	concurrency := w.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			w.claimLoop(ctx)
		}()
	}

	if interval := w.reclaimInterval(); interval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.reclaimLoop(ctx, interval)
		}()
	}

	if interval := w.metaloopInterval(); w.Metaloop != nil && interval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.metaloopLoop(ctx, interval)
		}()
	}

	wg.Wait()
	return nil
}

// claimLoop repeatedly claims and runs one request at a time until ctx is
// canceled; an empty claim backs off for PollInterval before trying again.
func (w *Worker) claimLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := w.RunOnce(ctx)
		if err != nil {
			w.logf("%v", err)
		}
		if claimed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.pollInterval()):
		}
	}
}

// RunOnce claims at most one pending request and, if one was claimed, runs
// it to completion — heartbeating throughout on a context detached from
// ctx's cancellation, so a caller canceling ctx never aborts in-flight
// work. Returns whether a request was claimed; exported for tests that
// drive a single claim cycle directly instead of the full Run loop.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	hr, err := w.Store.ClaimNext(ctx, w.ID)
	if err != nil {
		return false, fmt.Errorf("worker: claiming next request: %w", err)
	}
	if hr == nil {
		return false, nil
	}

	// Detached from ctx so a caller canceling ctx never aborts in-flight
	// work, but still cancelable: losing the claim to a reclaim must stop
	// this run rather than let two workers drive the same request.
	runCtx, lostClaim := context.WithCancel(context.WithoutCancel(ctx))
	defer lostClaim()
	stop := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		w.heartbeatUntil(runCtx, hr.ID, stop, lostClaim)
	}()

	runErr := w.runPipelineRecovered(runCtx, hr.ID)
	close(stop)
	hbWG.Wait()

	if runErr != nil {
		return true, fmt.Errorf("worker: running pipeline for request %s: %w", hr.ID, runErr)
	}
	return true, nil
}

// runPipelineRecovered runs the pipeline with a panic boundary: a bad
// request must fail its own claim, not crash the whole worker pool and
// every other in-flight request with it. A panicking claim is simply left
// running with its heartbeat stopped — the reclaim sweep picks it back up
// like any other stale claim, resuming at its last checkpoint.
func (w *Worker) runPipelineRecovered(ctx context.Context, requestID uuid.UUID) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.Pipeline.RunPipeline(ctx, requestID)
}

// heartbeatUntil refreshes id's heartbeat on a fixed interval until stop is
// closed. A refresh that touches no row means this worker is no longer the
// claimant; if the row has since been reclaimed by someone else,
// heartbeatUntil calls lostClaim to abort the in-flight pipeline so the two
// workers don't run it concurrently.
//
// A heartbeat that *errors* is treated the same way once the lease it was
// keeping alive has expired. The row's heartbeat_at stops advancing the moment
// the store call starts failing (a dropped connection, a paused database), so
// after StaleAfter any other instance's reclaim sweep is entitled to hand the
// request to a new claimant — while this one, which merely logged the error and
// carried on, is still running the same pipeline: two workers submitting to the
// judge under the shared system login and spending both model budgets twice.
// The store-side predicates catch that only after the side effects. Giving up
// at the lease boundary is the conservative half of the same rule the reclaim
// sweep follows.
func (w *Worker) heartbeatUntil(ctx context.Context, id uuid.UUID, stop <-chan struct{}, lostClaim context.CancelFunc) {
	ticker := time.NewTicker(w.heartbeatInterval())
	defer ticker.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ok, err := w.Store.Heartbeat(ctx, id, w.ID)
			if err != nil {
				w.logf("worker: heartbeat for request %s: %v", id, err)
				if time.Since(lastOK) >= w.staleAfter() {
					w.logf("worker: request %s has not been heartbeated for %s, lease expired, aborting run",
						id, w.staleAfter())
					lostClaim()
					return
				}
				continue
			}
			if ok {
				lastOK = time.Now()
				continue
			}
			// No row refreshed. That is the normal case once the pipeline
			// has reached a terminal status but hasn't returned yet, so
			// confirm the claim was actually taken from us before killing
			// the run.
			if w.claimLost(ctx, id) {
				w.logf("worker: request %s reclaimed by another worker, aborting run", id)
				lostClaim()
			}
			return
		}
	}
}

// claimLost reports whether id is still queued or running but no longer
// claimed by this worker — i.e. a reclaim sweep handed it to someone else.
// A terminal row, or a store error, is not treated as a lost claim: killing
// a run that is merely finishing would be worse than the extra tick.
func (w *Worker) claimLost(ctx context.Context, id uuid.UUID) bool {
	hr, err := w.Store.GetHelpRequest(ctx, id)
	if err != nil {
		w.logf("worker: checking claim for request %s: %v", id, err)
		return false
	}
	if hr == nil {
		return false
	}
	if hr.Status != store.StatusPending && hr.Status != store.StatusRunning {
		return false
	}
	return hr.ClaimedBy == nil || *hr.ClaimedBy != w.ID
}

// reclaimLoop runs a reclaim sweep on a fixed interval until ctx is
// canceled.
func (w *Worker) reclaimLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.reclaimSweep(ctx); err != nil {
				w.logf("reclaim sweep: %v", err)
			}
		}
	}
}

// metaloopLoop runs the curator sweep once at startup and then every
// interval until ctx is canceled.
//
// The startup sweep is not a nicety. A bare ticker fires only after a full
// interval has elapsed, so with the shipped 24h interval a service
// redeployed or restarted more often than once a day never ran the curator
// at all: raw_mistakes accumulated unbounded, mistakes was never populated,
// and the repair prompt's top-N — the read side the whole metaloop exists to
// feed — always rendered empty. The sweep is cheap when there is nothing to
// do (a user with no unprocessed raw mistakes costs zero model calls) and it
// runs on its own goroutine, so it delays nothing else.
func (w *Worker) metaloopLoop(ctx context.Context, interval time.Duration) {
	sweep := func() {
		if summary, err := w.Metaloop.Run(ctx); err != nil {
			w.logf("metaloop sweep: %v", err)
		} else {
			w.logf("metaloop sweep: processed %d user(s), merged %d, created %d, gave up on %d",
				summary.UsersProcessed, summary.Merged, summary.Created, summary.GaveUp)
		}
	}
	sweep()

	// A timer re-armed after each sweep, not a free-running ticker: a sweep
	// that outlasts the interval must not queue up a second one behind
	// itself.
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			sweep()
			timer.Reset(interval)
		}
	}
}

// reclaimSweep moves running rows whose heartbeat is older than StaleAfter
// back to pending.
func (w *Worker) reclaimSweep(ctx context.Context) ([]uuid.UUID, error) {
	ids, err := w.Store.ReclaimStale(ctx, w.staleAfter())
	if err != nil {
		return nil, fmt.Errorf("worker: reclaiming stale requests: %w", err)
	}
	return ids, nil
}

func (w *Worker) pollInterval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return defaultPollInterval
}

func (w *Worker) heartbeatInterval() time.Duration {
	if w.HeartbeatInterval > 0 {
		return w.HeartbeatInterval
	}
	return defaultHeartbeatInterval
}

func (w *Worker) staleAfter() time.Duration {
	if w.StaleAfter > 0 {
		return w.StaleAfter
	}
	return defaultStaleAfter
}

func (w *Worker) reclaimInterval() time.Duration {
	switch {
	case w.ReclaimInterval < 0:
		return 0
	case w.ReclaimInterval == 0:
		return defaultReclaimInterval
	default:
		return w.ReclaimInterval
	}
}

func (w *Worker) metaloopInterval() time.Duration {
	switch {
	case w.MetaloopInterval < 0:
		return 0
	case w.MetaloopInterval == 0:
		return defaultMetaloopInterval
	default:
		return w.MetaloopInterval
	}
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
