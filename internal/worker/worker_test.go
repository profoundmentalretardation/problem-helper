// Test isolation: shares cache_test.go's TestMain/testPool/withStore — same
// approach as internal/store/store_test.go, real dockerized Postgres
// (TEST_DATABASE_URL), each test in its own rolled-back transaction, EXCEPT
// TestWorker_ClaimNext_ExactlyOneWins, which needs two genuinely concurrent
// sessions to exercise FOR UPDATE SKIP LOCKED and so runs against the
// shared pool directly (auto-committing), cleaning up its own row.
package worker_test

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// fakePipelineRunner is a scriptable worker.PipelineRunner: each call
// records the request id it ran and, if release is non-nil, blocks until
// it's closed — used to hold a claim "in flight" while a test drives
// shutdown around it.
type fakePipelineRunner struct {
	started chan uuid.UUID // optional: signaled on every RunPipeline call
	release chan struct{}  // optional: RunPipeline blocks until closed

	mu  sync.Mutex
	ran []uuid.UUID
}

func (f *fakePipelineRunner) RunPipeline(_ context.Context, id uuid.UUID) error {
	if f.started != nil {
		f.started <- id
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	f.ran = append(f.ran, id)
	f.mu.Unlock()
	return nil
}

func (f *fakePipelineRunner) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ran)
}

// fakeQueueStore is an in-memory worker.QueueStore: a FIFO of claimable
// rows plus call logs, so claim-loop/heartbeat/shutdown behavior can be
// tested without a database.
type fakeQueueStore struct {
	mu         sync.Mutex
	pending    []*store.HelpRequest
	heartbeats []uuid.UUID

	// claimed tracks who the fake believes owns each row, so a test can
	// simulate a reclaim taking the claim away mid-run. Empty means every
	// heartbeat is accepted.
	claimed map[uuid.UUID]*store.HelpRequest

	// hbErr, when set, makes every Heartbeat call fail — a database the
	// worker cannot reach while its lease quietly expires.
	hbErr error
}

func (f *fakeQueueStore) ClaimNext(_ context.Context, _ string) (*store.HelpRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, nil
	}
	hr := f.pending[0]
	f.pending = f.pending[1:]
	return hr, nil
}

func (f *fakeQueueStore) Heartbeat(_ context.Context, id uuid.UUID, workerID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, id)
	if f.hbErr != nil {
		return false, f.hbErr
	}
	if f.claimed == nil {
		return true, nil
	}
	hr, ok := f.claimed[id]
	if !ok || hr.ClaimedBy == nil || *hr.ClaimedBy != workerID {
		return false, nil
	}
	return true, nil
}

func (f *fakeQueueStore) GetHelpRequest(_ context.Context, id uuid.UUID) (*store.HelpRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimed[id], nil
}

func (f *fakeQueueStore) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heartbeats)
}

func (f *fakeQueueStore) ReclaimStale(_ context.Context, _ time.Duration) ([]uuid.UUID, error) {
	return nil, nil
}

func TestWorker_ClaimNext_ExactlyOneWins(t *testing.T) {
	lockQueueTable(t)
	ctx := context.Background()

	committed := store.New(testPool)
	id := uuid.New()
	if err := committed.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID: id, UserID: "user-1", ProblemID: "problem-1", Platform: "mock", NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM help_requests WHERE id = $1`, id)
	})

	tx1, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	tx2, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()

	s1 := store.WithTx(tx1)
	s2 := store.WithTx(tx2)

	results := make([]*store.HelpRequest, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = s1.ClaimNext(ctx, "worker-1")
	}()
	go func() {
		defer wg.Done()
		results[1], errs[1] = s2.ClaimNext(ctx, "worker-2")
	}()
	wg.Wait()

	if errs[0] != nil {
		t.Fatalf("worker-1 claim: %v", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("worker-2 claim: %v", errs[1])
	}

	claimed := 0
	for _, r := range results {
		if r != nil {
			claimed++
			if r.ID != id {
				t.Errorf("claimed id = %s, want %s", r.ID, id)
			}
		}
	}
	if claimed != 1 {
		t.Fatalf("want exactly one of two concurrent claimants to win, got %d", claimed)
	}
}

func TestWorker_ReclaimedRequest_IsReClaimedAndRun(t *testing.T) {
	lockQueueTable(t)
	s, ctx := withStore(t)
	id := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID: id, UserID: "user-1", ProblemID: "problem-1", Platform: "mock", NSubmissionsTaken: 5,
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	if _, err := s.ClaimNext(ctx, "dead-worker"); err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if err := s.SetResumeStep(ctx, id, "", "shield"); err != nil {
		t.Fatalf("set resume step: %v", err)
	}

	// Simulate the claimant crashing well past staleness: reclaim with a
	// cutoff safely in the future so the freshly-set heartbeat still
	// counts as stale.
	reclaimed, err := s.ReclaimStale(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("reclaim stale: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != id {
		t.Fatalf("reclaimed = %v, want [%s]", reclaimed, id)
	}

	got, err := s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusPending {
		t.Fatalf("status after reclaim = %s, want pending", got.Status)
	}
	if got.ResumeStep == nil || *got.ResumeStep != "shield" {
		t.Fatalf("resume_step after reclaim = %v, want it preserved as %q (not restarted from step 1)", got.ResumeStep, "shield")
	}

	fp := &fakePipelineRunner{}
	w := &worker.Worker{ID: "worker-2", Store: s, Pipeline: fp}
	claimed, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatal("run once: want the reclaimed row to be claimed")
	}

	got, err = s.GetHelpRequest(ctx, id)
	if err != nil {
		t.Fatalf("get help request: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Fatalf("status after re-claim = %s, want running", got.Status)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != "worker-2" {
		t.Errorf("claimed_by = %v, want worker-2", got.ClaimedBy)
	}
	if got.ResumeStep == nil || *got.ResumeStep != "shield" {
		t.Errorf("resume_step after re-claim = %v, want it still %q", got.ResumeStep, "shield")
	}
	if fp.runCount() != 1 || fp.ran[0] != id {
		t.Errorf("pipeline runs = %v, want exactly [%s]", fp.ran, id)
	}
}

func TestWorker_GracefulShutdown_WaitsForInFlightRequest(t *testing.T) {
	id := uuid.New()
	fq := &fakeQueueStore{pending: []*store.HelpRequest{{ID: id}}}
	started := make(chan uuid.UUID, 1)
	release := make(chan struct{})
	fp := &fakePipelineRunner{started: started, release: release}

	w := &worker.Worker{
		ID:                "w1",
		Store:             fq,
		Pipeline:          fp,
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		ReclaimInterval:   -1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	select {
	case gotID := <-started:
		if gotID != id {
			t.Fatalf("started id = %s, want %s", gotID, id)
		}
	case <-time.After(time.Second):
		t.Fatal("pipeline never started")
	}

	if fq.heartbeatCount() == 0 {
		// Heartbeats are async on a 5ms tick; give the ticker a moment.
		time.Sleep(20 * time.Millisecond)
	}
	if fq.heartbeatCount() == 0 {
		t.Error("no heartbeat recorded for the in-flight request")
	}

	cancel()

	select {
	case <-done:
		t.Fatal("Run returned before the in-flight pipeline finished — SIGTERM must not abort in-flight work")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the in-flight pipeline finished")
	}

	if fp.runCount() != 1 || fp.ran[0] != id {
		t.Fatalf("pipeline runs = %v, want exactly [%s]", fp.ran, id)
	}

	hbAfterDone := fq.heartbeatCount()
	time.Sleep(30 * time.Millisecond)
	if fq.heartbeatCount() != hbAfterDone {
		t.Error("heartbeats kept arriving after Run returned — the ticker must stop with the pipeline")
	}
}

// ctxWatchingPipeline blocks until its run context is canceled, so a test
// can observe whether the worker aborts an in-flight run.
type ctxWatchingPipeline struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{} // optional: also returns when closed
}

func (p *ctxWatchingPipeline) RunPipeline(ctx context.Context, _ uuid.UUID) error {
	close(p.started)
	select {
	case <-ctx.Done():
		close(p.canceled)
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

// If a worker's heartbeats lapse long enough for the reclaim sweep to hand
// its request to another worker, it has to stop — otherwise two workers run
// the same pipeline concurrently, each submitting to the judge as the
// system user and each spending the model budget in full.
func TestWorker_AbortsRunAfterLosingItsClaim(t *testing.T) {
	id := uuid.New()
	other := "worker-2"
	fq := &fakeQueueStore{
		pending: []*store.HelpRequest{{ID: id, Status: store.StatusRunning}},
		// The row is already owned by someone else, so the first heartbeat
		// finds it taken.
		claimed: map[uuid.UUID]*store.HelpRequest{
			id: {ID: id, Status: store.StatusRunning, ClaimedBy: &other},
		},
	}
	fp := &ctxWatchingPipeline{
		started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}

	w := &worker.Worker{
		ID:                "worker-1",
		Store:             fq,
		Pipeline:          fp,
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		ReclaimInterval:   -1,
		MetaloopInterval:  -1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	select {
	case <-fp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never started")
	}
	select {
	case <-fp.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker kept running a request it no longer owns")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// The counterpart: while the worker still owns the row, a heartbeat that
// touches no row must not kill the run — and the ordinary case must not be
// disturbed at all.
func TestWorker_KeepsRunningWhileItStillOwnsTheClaim(t *testing.T) {
	id := uuid.New()
	me := "worker-1"
	fq := &fakeQueueStore{
		pending: []*store.HelpRequest{{ID: id, Status: store.StatusRunning}},
		claimed: map[uuid.UUID]*store.HelpRequest{
			id: {ID: id, Status: store.StatusRunning, ClaimedBy: &me},
		},
	}
	release := make(chan struct{})
	fp := &ctxWatchingPipeline{
		started: make(chan struct{}), canceled: make(chan struct{}), release: release,
	}

	w := &worker.Worker{
		ID:                me,
		Store:             fq,
		Pipeline:          fp,
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		ReclaimInterval:   -1,
		MetaloopInterval:  -1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	select {
	case <-fp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never started")
	}
	select {
	case <-fp.canceled:
		t.Fatal("worker aborted a run it still owns")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A heartbeat that keeps erroring is not a heartbeat: heartbeat_at stops
// advancing, so after StaleAfter another instance's reclaim sweep may hand the
// request to a new claimant. A worker that only logged the error and kept
// going would then be running the same pipeline as the new claimant —
// submitting to the judge under the shared system login and spending both
// model budgets twice. It has to give up its own run at the lease boundary.
func TestWorker_AbortsTheRunWhenHeartbeatsKeepFailing(t *testing.T) {
	id := uuid.New()
	fq := &fakeQueueStore{
		pending: []*store.HelpRequest{{ID: id, Status: store.StatusRunning}},
		hbErr:   errors.New("connection refused"),
	}
	release := make(chan struct{})
	defer close(release)
	fp := &ctxWatchingPipeline{
		started: make(chan struct{}), canceled: make(chan struct{}), release: release,
	}

	w := &worker.Worker{
		ID:                "worker-1",
		Store:             fq,
		Pipeline:          fp,
		Logger:            log.New(io.Discard, "", 0),
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		StaleAfter:        50 * time.Millisecond,
		ReclaimInterval:   -1,
		MetaloopInterval:  -1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	select {
	case <-fp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never started")
	}

	start := time.Now()
	select {
	case <-fp.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker kept running with a lease it could no longer refresh")
	}
	// The other direction: it must not bail on the first failed tick either,
	// or one blip on the database kills a run that was never reclaimed.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("aborted after %s, want the run to survive until the %s lease expires", elapsed, w.StaleAfter)
	}
}

// fakeMetaloopRunner is a scriptable worker.MetaloopRunner: it just counts
// calls, for tests that assert the cron loop fires and stops with Run.
type fakeMetaloopRunner struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeMetaloopRunner) Run(_ context.Context) (worker.MetaloopSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return worker.MetaloopSummary{}, nil
}

func (f *fakeMetaloopRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestWorker_MetaloopRunsOnInterval_AndStopsWithContext(t *testing.T) {
	fq := &fakeQueueStore{}
	fp := &fakePipelineRunner{}
	fm := &fakeMetaloopRunner{}

	w := &worker.Worker{
		ID:                "w1",
		Store:             fq,
		Pipeline:          fp,
		Metaloop:          fm,
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		ReclaimInterval:   -1,
		MetaloopInterval:  10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Two sweeps, not one: the first is the startup sweep, so only a second
	// proves the interval is still arming.
	deadline := time.Now().Add(time.Second)
	for fm.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fm.callCount() < 2 {
		t.Fatalf("metaloop ran %d time(s), want it to keep firing on its interval", fm.callCount())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}

	seen := fm.callCount()
	time.Sleep(30 * time.Millisecond)
	if fm.callCount() != seen {
		t.Error("metaloop kept firing after Run returned — the ticker must stop with the worker")
	}
}

// A bare ticker fires only after a full interval, so with the shipped 24h
// interval a service redeployed more often than once a day never ran the
// curator at all: raw_mistakes accumulated forever, mistakes stayed empty,
// and the repair prompt's top-N — the read side of the whole metaloop —
// always rendered nothing. The first sweep is capped at
// one sweep at startup instead.
func TestWorker_MetaloopRunsAtStartup_NotOnlyAfterAFullInterval(t *testing.T) {
	fq := &fakeQueueStore{}
	fp := &fakePipelineRunner{}
	fm := &fakeMetaloopRunner{}

	w := &worker.Worker{
		ID:                "w1",
		Store:             fq,
		Pipeline:          fp,
		Metaloop:          fm,
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
		ReclaimInterval:   -1,
		// Far longer than the test's patience: only the startup sweep can
		// make this fire.
		MetaloopInterval: time.Hour,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for fm.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fm.callCount() == 0 {
		t.Fatal("metaloop never ran: a process restarted more often than the interval never curates at all")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWorker_MetaloopNil_NoCronGoroutine(t *testing.T) {
	fq := &fakeQueueStore{}
	fp := &fakePipelineRunner{}

	w := &worker.Worker{
		ID:               "w1",
		Store:            fq,
		Pipeline:         fp,
		Concurrency:      1,
		PollInterval:     5 * time.Millisecond,
		ReclaimInterval:  -1,
		MetaloopInterval: 5 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel (nil Metaloop should not start a cron goroutine)")
	}
}
