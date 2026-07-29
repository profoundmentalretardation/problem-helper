// Test isolation: same approach as internal/store/store_test.go — real,
// dockerized Postgres (TEST_DATABASE_URL, default
// postgres://helper:helper@localhost:5432/helper?sslmode=disable), migrated
// once in TestMain, each test bound to its own rolled-back transaction via
// store.WithTx so tests never observe each other's writes. The model is
// scripted (llm.Scripted): no API key needed, same pattern as repair/hint.
package curator_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/curator"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
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
		panic("curator_test: cannot reach test postgres at " + dsn + ": " + err.Error())
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

func createRequest(t *testing.T, s *store.Store, ctx context.Context, userID string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := s.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:        id,
		UserID:    userID,
		ProblemID: "problem-1",
		Platform:  "mock",
	}); err != nil {
		t.Fatalf("create help request: %v", err)
	}
	return id
}

func testTemplate(t *testing.T) prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse("curator", "RAW:\n{{raw_mistakes}}\n\nEXISTING:\n{{existing_mistakes}}\n")
	if err != nil {
		t.Fatalf("parsing template: %v", err)
	}
	return tmpl
}

func testAgent(maxRetries int) config.AgentConfig {
	return config.AgentConfig{Model: "curator-model", MaxRetries: maxRetries}
}

func testPricing() map[string]config.PricingConfig {
	return map[string]config.PricingConfig{
		"curator-model": {Input: 1, CachedInput: 0.1, Output: 2},
	}
}

func newRunner(s *store.Store, chat llm.ChatClient, maxRetries int, t *testing.T) *curator.Runner {
	return &curator.Runner{
		Chat:        chat,
		RawMistakes: s,
		Mistakes:    s,
		Template:    testTemplate(t),
		Agent:       testAgent(maxRetries),
	}
}

func mergeJSON(mistakeID uuid.UUID) string {
	return fmt.Sprintf(`{"action":"merge_into","mistake_id":%q}`, mistakeID)
}

func createJSON(title, description string) string {
	return fmt.Sprintf(`{"action":"create_mistake","title":%q,"description":%q}`, title, description)
}

const finishJSON = `{"action":"finish"}`

func TestRun_NoUnprocessed_ZeroModelCalls(t *testing.T) {
	s, ctx := withStore(t)

	chat := llm.NewScripted(nil, testPricing()) // no scripted responses: any Chat call panics
	runner := newRunner(s, chat, 4, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusNoUnprocessed {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusNoUnprocessed)
	}
	if got.Calls != 0 {
		t.Errorf("calls = %d, want 0", got.Calls)
	}
}

func TestRun_MergeInto_NearDuplicate(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	existingID := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{
		ID:          existingID,
		UserID:      "user-1",
		Title:       "off-by-one",
		Description: "loop bound is one short",
	}); err != nil {
		t.Fatalf("create existing mistake: %v", err)
	}
	before, err := s.ListMistakes(ctx, "user-1")
	if err != nil || len(before) != 1 {
		t.Fatalf("list mistakes before merge: %+v, %v", before, err)
	}

	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "loop stops one iteration early again",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	// Ensure last_seen can move forward even at low clock resolution.
	time.Sleep(5 * time.Millisecond)

	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: mergeJSON(existingID), Usage: llm.Usage{InputTokens: 40, OutputTokens: 10}},
		llm.ScriptedResponse{JSON: finishJSON, Usage: llm.Usage{InputTokens: 20, OutputTokens: 5}},
	)
	runner := newRunner(s, chat, 4, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusCurated {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusCurated)
	}
	if got.Merged != 1 || got.Created != 0 {
		t.Errorf("merged=%d created=%d, want merged=1 created=0", got.Merged, got.Created)
	}

	after, err := s.ListMistakes(ctx, "user-1")
	if err != nil || len(after) != 1 {
		t.Fatalf("list mistakes after merge: %+v, %v", after, err)
	}
	if after[0].Count != before[0].Count+1 {
		t.Errorf("count = %d, want %d", after[0].Count, before[0].Count+1)
	}
	if !after[0].LastSeen.After(before[0].LastSeen) {
		t.Errorf("last_seen = %v, want it to move forward from %v", after[0].LastSeen, before[0].LastSeen)
	}

	unprocessed, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("unprocessed raw mistakes = %+v, want none (finish marks the batch processed)", unprocessed)
	}

	if chat.Remaining() != 0 {
		t.Errorf("scripted responses left unused: %d", chat.Remaining())
	}
}

func TestRun_CreateMistake_GenuinelyNew(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "forgets to flush stdout before reading",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{
			JSON:  createJSON("stdout flushing", "Forgets to flush stdout before the next read, so output is missing or out of order."),
			Usage: llm.Usage{InputTokens: 40, OutputTokens: 10},
		},
		llm.ScriptedResponse{JSON: finishJSON, Usage: llm.Usage{InputTokens: 20, OutputTokens: 5}},
	)
	runner := newRunner(s, chat, 4, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusCurated {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusCurated)
	}
	if got.Created != 1 || got.Merged != 0 {
		t.Errorf("created=%d merged=%d, want created=1 merged=0", got.Created, got.Merged)
	}

	mistakes, err := s.ListMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list mistakes: %v", err)
	}
	if len(mistakes) != 1 {
		t.Fatalf("mistakes = %+v, want exactly one new row", mistakes)
	}
	if mistakes[0].Title != "stdout flushing" || mistakes[0].Count != 1 {
		t.Errorf("unexpected mistake row: %+v", mistakes[0])
	}

	unprocessed, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("unprocessed raw mistakes = %+v, want none", unprocessed)
	}
}

func TestRun_Garbage_NothingWrittenStaysUnprocessed(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "some observation",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	// Every reply is unparseable prose; the budget — one call per raw
	// mistake (1) plus MaxRetries (2) of slack, so 3 — is exhausted before
	// any tool ever runs.
	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `not json at all`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: `still not json`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		llm.ScriptedResponse{JSON: `prose to the very end`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	)
	runner := newRunner(s, chat, 2, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusGaveUp {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusGaveUp)
	}
	if got.Merged != 0 || got.Created != 0 {
		t.Errorf("merged=%d created=%d, want both 0", got.Merged, got.Created)
	}

	mistakes, err := s.ListMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list mistakes: %v", err)
	}
	if len(mistakes) != 0 {
		t.Errorf("mistakes = %+v, want none written", mistakes)
	}

	unprocessed, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if len(unprocessed) != 1 {
		t.Errorf("unprocessed raw mistakes = %+v, want the row still unprocessed (retried next night)", unprocessed)
	}

	if chat.Remaining() != 0 {
		t.Errorf("scripted responses left unused: %d", chat.Remaining())
	}
}

// max_cost_per_loop is declared for the curator in agents.yaml and
// validated at startup, so it has to actually bound a sweep — otherwise a
// user with a long backlog can run up an unbounded bill overnight, with
// nothing in config to stop it.
func TestRun_MaxCostPerLoop_StopsBeforeNextCall(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	for i := 0; i < 3; i++ {
		if err := s.InsertRawMistake(ctx, store.RawMistake{
			RequestID: reqID, UserID: "user-1", Text: fmt.Sprintf("observation %d", i),
		}); err != nil {
			t.Fatalf("insert raw mistake: %v", err)
		}
	}

	// The call budget alone (3 raw + 2 slack = 5) would allow all four
	// replies; the cost cap must stop the loop after the first call, whose
	// cost already exceeds it.
	usage := llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: createJSON("off-by-one", "loop bound is one short"), Usage: usage},
		llm.ScriptedResponse{JSON: createJSON("another", "another habit"), Usage: usage},
		llm.ScriptedResponse{JSON: createJSON("third", "third habit"), Usage: usage},
		llm.ScriptedResponse{JSON: finishJSON, Usage: usage},
	)
	runner := newRunner(s, chat, 2, t)
	runner.Agent.MaxCostPerLoop = 1.0

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusGaveUp {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusGaveUp)
	}
	if got.Calls != 1 {
		t.Errorf("calls = %d, want 1 (the cap is checked before each call, so it overshoots by at most one)", got.Calls)
	}

	// finish was never reached, but that first call's create_mistake is
	// committed — CreateMistake writes as it goes. Leaving the batch
	// unprocessed would make the next sweep re-send the identical raw
	// mistakes and create the same mistake again, inflating mistakes.count
	// (which drives the repair prompt's top-N) every night without bound.
	if got.Created != 1 {
		t.Fatalf("created = %d, want 1", got.Created)
	}
	unprocessed, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("unprocessed = %d, want 0 (a give-up that wrote something still consumes its batch)", len(unprocessed))
	}
}

// The other half of the rule: a sweep that gave up without writing anything
// leaves the batch alone, so pure garbage from the model costs a retry
// rather than the student's whole backlog.
func TestRun_GaveUpWithoutWriting_LeavesBatchUnprocessed(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "observation",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	// Budget is 1 raw + 1 slack = 2 calls, both of them unusable replies.
	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: `{"action":"nonsense"}`},
		llm.ScriptedResponse{JSON: `{"action":"nonsense"}`},
	)
	runner := newRunner(s, chat, 1, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != curator.StatusGaveUp {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusGaveUp)
	}
	if got.Merged != 0 || got.Created != 0 {
		t.Fatalf("merged/created = %d/%d, want 0/0", got.Merged, got.Created)
	}
	unprocessed, err := s.ListUnprocessedRawMistakes(ctx, "user-1")
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if len(unprocessed) != 1 {
		t.Errorf("unprocessed = %d, want 1 (nothing was written, so the batch is retried next sweep)", len(unprocessed))
	}
}

func TestRun_RendersRawAndExistingMistakesIntoThePrompt(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	existingID := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{
		ID: existingID, UserID: "user-1", Title: "off-by-one", Description: "loop bound is one short",
	}); err != nil {
		t.Fatalf("create existing mistake: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "loop stops one iteration early again",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	chat := llm.NewScripted(nil, testPricing(),
		llm.ScriptedResponse{JSON: finishJSON, Usage: llm.Usage{InputTokens: 20, OutputTokens: 5}},
	)
	runner := newRunner(s, chat, 4, t)

	if _, err := runner.Run(ctx, "user-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := chat.Calls()
	if len(calls) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(calls))
	}
	system := calls[0].Messages[0].Content
	if !strings.Contains(system, "loop stops one iteration early again") {
		t.Errorf("system prompt missing the unprocessed raw mistake: %q", system)
	}
	if !strings.Contains(system, "off-by-one") || !strings.Contains(system, existingID.String()) {
		t.Errorf("system prompt missing the existing mistake or its id: %q", system)
	}
}

// TestRun_MergeInto_OtherUsersMistake_FedBackNotFatal covers the model
// naming a mistake that isn't this student's — a hallucinated uuid, or one
// belonging to another student. It must come back as a tool error the model
// can recover from, not abort the sweep (which would strand the batch, since
// the next sweep re-sends the same prompt), and it must not touch the other
// student's tally.
func TestRun_MergeInto_OtherUsersMistake_FedBackNotFatal(t *testing.T) {
	s, ctx := withStore(t)
	reqID := createRequest(t, s, ctx, "user-1")

	othersID := uuid.New()
	if err := s.CreateMistake(ctx, store.Mistake{
		ID: othersID, UserID: "user-2", Title: "theirs", Description: "not user-1's habit",
	}); err != nil {
		t.Fatalf("create other user's mistake: %v", err)
	}
	if err := s.InsertRawMistake(ctx, store.RawMistake{
		RequestID: reqID, UserID: "user-1", Text: "loop stops one iteration early",
	}); err != nil {
		t.Fatalf("insert raw mistake: %v", err)
	}

	chat := llm.NewScripted(nil, testPricing(),
		// First the model reaches for a mistake that isn't user-1's...
		llm.ScriptedResponse{JSON: mergeJSON(othersID), Usage: llm.Usage{InputTokens: 40, OutputTokens: 10}},
		// ...then recovers by creating one, and finishes.
		llm.ScriptedResponse{
			JSON:  `{"action":"create_mistake","title":"off-by-one","description":"loop bound is one short"}`,
			Usage: llm.Usage{InputTokens: 40, OutputTokens: 10},
		},
		llm.ScriptedResponse{JSON: finishJSON, Usage: llm.Usage{InputTokens: 20, OutputTokens: 5}},
	)
	runner := newRunner(s, chat, 4, t)

	got, err := runner.Run(ctx, "user-1")
	if err != nil {
		t.Fatalf("Run: %v (an unknown mistake id must not fail the sweep)", err)
	}
	if got.Status != curator.StatusCurated {
		t.Fatalf("status = %q, want %q", got.Status, curator.StatusCurated)
	}
	if got.Merged != 0 || got.Created != 1 {
		t.Errorf("merged=%d created=%d, want merged=0 created=1", got.Merged, got.Created)
	}

	theirs, err := s.ListMistakes(ctx, "user-2")
	if err != nil {
		t.Fatalf("list user-2 mistakes: %v", err)
	}
	if len(theirs) != 1 || theirs[0].Count != 1 {
		t.Errorf("user-2 mistakes = %+v, want count unchanged at 1", theirs)
	}

	if chat.Remaining() != 0 {
		t.Errorf("scripted responses left unused: %d", chat.Remaining())
	}
}
