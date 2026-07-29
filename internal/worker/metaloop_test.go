package worker_test

import (
	"context"
	"sync"
	"testing"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/curator"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// fakeUserLister returns a canned worklist for Metaloop.
type fakeUserLister struct{ users []string }

func (f *fakeUserLister) ListUsersWithUnprocessedMistakes(_ context.Context) ([]string, error) {
	return f.users, nil
}

// blockingCurator records the users it was asked to curate and can hold the
// first call open, so a second sweep can be observed racing the first.
type blockingCurator struct {
	mu      sync.Mutex
	seen    []string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCurator) Run(_ context.Context, userID string) (curator.Result, error) {
	b.mu.Lock()
	b.seen = append(b.seen, userID)
	first := len(b.seen) == 1
	b.mu.Unlock()
	if first && b.entered != nil {
		close(b.entered)
		<-b.release
	}
	return curator.Result{}, nil
}

func (b *blockingCurator) users() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// TryRun must decline rather than queue: sync.Mutex.Lock ignores the context,
// so an admin trigger waiting on an in-progress sweep is an unkillable
// goroutine that then re-runs the batches the sweep ahead of it just did.
func TestMetaloop_TryRunDeclinesWhileASweepIsRunning(t *testing.T) {
	cur := &blockingCurator{entered: make(chan struct{}), release: make(chan struct{})}
	m := &worker.Metaloop{Store: &fakeUserLister{users: []string{"alice", "bob"}}, Curator: cur}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Run(context.Background()); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	<-cur.entered

	if _, ran, err := m.TryRun(context.Background()); err != nil {
		t.Fatalf("TryRun: %v", err)
	} else if ran {
		t.Fatal("TryRun claimed to sweep while another sweep held the lock")
	}

	close(cur.release)
	<-done
}

// A sweep whose context is already done must stop, not walk the rest of the
// worklist making calls whose first store query fails immediately — thousands
// of useless log lines and round trips at exactly the moment the process is
// shutting down.
func TestMetaloop_RunStopsWhenTheContextIsDone(t *testing.T) {
	cur := &blockingCurator{}
	m := &worker.Metaloop{
		Store:   &fakeUserLister{users: []string{"alice", "bob", "carol"}},
		Curator: cur,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cur.users(); len(got) != 0 {
		t.Errorf("curator was called for %v, want no calls on a cancelled sweep", got)
	}
	if summary.UsersProcessed != 0 {
		t.Errorf("UsersProcessed = %d, want 0", summary.UsersProcessed)
	}
}
