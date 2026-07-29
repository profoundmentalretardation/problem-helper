// Test isolation: same approach as internal/store/store_test.go — real,
// dockerized Postgres (TEST_DATABASE_URL, default
// postgres://helper:helper@localhost:5432/helper?sslmode=disable), migrated
// once in TestMain, each test bound to its own rolled-back transaction via
// store.WithTx so tests never observe each other's writes.
package worker_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://helper:helper@localhost:5432/helper?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		panic(err)
	}
	if err := pool.Ping(ctx); err != nil {
		panic("worker_test: cannot reach test postgres at " + dsn + ": " + err.Error())
	}
	if err := store.Migrate(ctx, pool); err != nil {
		panic(err)
	}
	testPool = pool

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func withStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
	})

	return store.WithTx(tx), ctx
}

func createRequest(t *testing.T, s *store.Store, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:        id,
		UserID:    "user-1",
		ProblemID: "problem-1",
		Platform:  "mock",
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	return id
}

func TestLookup_Hit(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx)

	hash := worker.HashCode("int main() { return 0; }")
	if err := s.InsertHint(ctx, store.Hint{
		RequestID: reqID,
		ProblemID: "problem-1",
		CodeHash:  hash,
		Text:      "check your return value",
		Approved:  true,
	}); err != nil {
		t.Fatalf("insert hint: %v", err)
	}

	got, ok := worker.Lookup(ctx, s, "problem-1", hash)
	if !ok {
		t.Fatal("Lookup: ok = false, want true")
	}
	if got.Text != "check your return value" {
		t.Errorf("Lookup: text = %q, want %q", got.Text, "check your return value")
	}
}

func TestLookup_Miss(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx)

	hash := worker.HashCode("int main() { return 0; }")
	if err := s.InsertHint(ctx, store.Hint{
		RequestID: reqID,
		ProblemID: "problem-1",
		CodeHash:  hash,
		Text:      "an unapproved hint",
		Approved:  false,
	}); err != nil {
		t.Fatalf("insert unapproved hint: %v", err)
	}

	tests := []struct {
		name      string
		problemID string
		codeHash  string
	}{
		{"unapproved hint", "problem-1", hash},
		{"different hash", "problem-1", worker.HashCode("int main() { return 1; }")},
		{"different problem", "problem-2", hash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := worker.Lookup(ctx, s, tt.problemID, tt.codeHash); ok {
				t.Errorf("Lookup: ok = true, got %+v, want a miss", got)
			}
		})
	}
}

func TestHashCode_NormalizesWhitespace(t *testing.T) {
	base := worker.HashCode("int main() {\n    return 0;\n}")

	tests := []struct {
		name string
		code string
	}{
		{"trailing newline", "int main() {\n    return 0;\n}\n"},
		{"leading/trailing blank lines", "\n\nint main() {\n    return 0;\n}\n\n"},
		{"CRLF line endings", "int main() {\r\n    return 0;\r\n}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := worker.HashCode(tt.code); got != base {
				t.Errorf("HashCode(%q) = %q, want %q (same as base)", tt.code, got, base)
			}
		})
	}
}

func TestHashCode_DifferentCodeDiffers(t *testing.T) {
	a := worker.HashCode("int main() { return 0; }")
	b := worker.HashCode("int main() { return 1; }")
	if a == b {
		t.Error("HashCode: different code hashed to the same value")
	}
}
