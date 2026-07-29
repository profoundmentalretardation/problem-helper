// Package store is the Postgres-backed persistence layer: schema migrations,
// help_requests status transitions, submission snapshots, and the
// append-only events/llm_calls logs.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/migrations"
)

// Status is a help_requests.status value.
type Status string

const (
	StatusPending       Status = "pending"
	StatusRunning       Status = "running"
	StatusAlreadySolved Status = "already_solved"
	StatusNoSubmissions Status = "no_submissions"
	StatusDone          Status = "done"
	StatusNoFix         Status = "no_fix"
	StatusNoHint        Status = "no_hint"
	StatusFailed        Status = "failed"
)

// legalTransitions is the status graph from the plan's Technical Details:
// pending -> running -> {terminal...}; running -> pending is the crash
// reclaim path. Any status absent as a key is terminal.
var legalTransitions = map[Status][]Status{
	StatusPending: {StatusRunning},
	StatusRunning: {
		StatusAlreadySolved,
		StatusNoSubmissions,
		StatusDone,
		StatusNoFix,
		StatusNoHint,
		StatusFailed,
		StatusPending,
	},
}

// ErrDuplicateRequest is returned when a help_requests row with the same id
// already exists.
var ErrDuplicateRequest = errors.New("store: help request already exists")

// ErrUnknownRequest is returned when an operation references a request_id
// that has no help_requests row.
var ErrUnknownRequest = errors.New("store: unknown request")

// ErrClaimLost is returned when a worker tries to write a terminal status
// onto a request that has since been reclaimed by another worker. It is
// deliberately distinct from ErrIllegalTransition: the transition was legal,
// the row simply is not this worker's to finish any more.
var ErrClaimLost = errors.New("store: request claim lost to another worker")

// ErrIllegalTransition is returned when a status transition is not allowed
// by the graph in the plan's Technical Details.
type ErrIllegalTransition struct {
	From, To Status
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("store: illegal status transition from %q to %q", e.From, e.To)
}

// dbtx is satisfied by *pgxpool.Pool, *pgxpool.Conn and pgx.Tx, so Store can
// run either against the pool directly or against a single transaction
// (see store_test.go for the test-isolation rationale).
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the persistence layer, bound to either a pool or a transaction.
type Store struct {
	db dbtx
}

// New wraps a connected pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{db: pool}
}

// WithTx returns a Store bound to tx instead of the original pool/conn —
// used by tests to isolate each test in a rolled-back transaction.
func WithTx(tx pgx.Tx) *Store {
	return &Store{db: tx}
}

// migrateLockKey serializes Migrate across processes. The check ("is this
// migration recorded?") and the apply are two separate statements, so two
// callers starting at once — two service instances rolling out together, or
// `go test ./...` running the store and worker package binaries against the
// same TEST_DATABASE_URL — both see a new migration as unapplied and both
// run it. The second one fails on whatever the first already created, taking
// a healthy process down at startup.
const migrateLockKey = 8829471

// Migrate applies every migrations/*.sql file, in filename order, that has
// not already been recorded in schema_migrations. Safe to call repeatedly
// and safe to call concurrently: it holds a session-level advisory lock for
// the whole scan-and-apply, so a second caller waits and then finds every
// migration already recorded.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquiring migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("store: locking migrations: %w", err)
	}
	defer func() {
		// Unlock on the same session that locked; the lock is released by the
		// connection dropping anyway, but not while it sits in the pool.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	// Every statement below runs on conn, the connection holding the lock —
	// not on pool. Going back to the pool for them deadlocks a deployment
	// whose DATABASE_URL caps the pool at one connection: conn is checked out
	// for the whole function, so pool.Exec would wait for a connection that
	// only becomes free once Migrate returns.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name        TEXT PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("store: reading migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("store: checking migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: reading migration %s: %w", name, err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: beginning migration tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: applying migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: recording migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: committing migration %s: %w", name, err)
		}
	}
	return nil
}

// HelpRequestInput creates a help_requests row; the caller supplies ID
// (generated by the API layer) since request_id is returned synchronously
// from POST /help before the worker ever runs.
type HelpRequestInput struct {
	ID                uuid.UUID
	UserID            string
	ProblemID         string
	Platform          string
	NSubmissionsTaken int
}

// HelpRequest is a help_requests row.
type HelpRequest struct {
	ID                uuid.UUID
	UserID            string
	ProblemID         string
	Platform          string
	NSubmissionsTaken int
	Status            Status
	FailureReason     *string
	BestSubmissionID  *uuid.UUID
	HintID            *uuid.UUID
	Useless           bool
	Error             *string
	ClaimedBy         *string
	HeartbeatAt       *time.Time
	ResumeStep        *string
	// RepairCode / RepairRunID hold the verified fix from loop 1 and the
	// judge run it was accepted on. They are what makes the "repair" resume
	// checkpoint honourable: the hint loop needs the working code, so
	// without them a reclaimed row has no choice but to re-run loop 1.
	RepairCode  *string
	RepairRunID *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateHelpRequest inserts a new help_requests row with status=pending.
func (s *Store) CreateHelpRequest(ctx context.Context, in HelpRequestInput) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO help_requests (id, user_id, problem_id, platform, n_submissions_taken, status)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		in.ID, in.UserID, in.ProblemID, in.Platform, in.NSubmissionsTaken, StatusPending,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: id %s", ErrDuplicateRequest, in.ID)
		}
		return fmt.Errorf("store: creating help request: %w", err)
	}
	return nil
}

// CreateHelpRequestWithinDailyLimit inserts a new pending help_requests row
// only if the user has fewer than limit rows created at or after since,
// reporting whether the row was actually inserted (false = over the limit).
//
// The count and the insert run inside one transaction, behind a per-user
// advisory lock taken first. Counting and inserting as two unsynchronized
// round trips lets N concurrent POST /help for the same user all read the
// same pre-insert count and all pass — and daily_requests_per_user is the
// only thing standing between one caller and both the whole LLM budget and a
// flood of judge submissions under the shared system login. No unusual
// timing is needed; any burst from a frontend defeats it.
//
// A single `INSERT ... SELECT ... WHERE (SELECT count(*) ...) < limit` does
// not fix that either, which is the non-obvious part: under READ COMMITTED
// the subquery reads the statement's own snapshot, so concurrent inserts are
// invisible to it and every racing statement still sees the same count. The
// lock has to be held across a fresh snapshot, which means an explicit
// transaction with the count as its own statement.
func (s *Store) CreateHelpRequestWithinDailyLimit(
	ctx context.Context, in HelpRequestInput, since time.Time, limit int,
) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: beginning rate-limit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Scoped to the user, so one student's burst never blocks another's.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, in.UserID,
	); err != nil {
		return false, fmt.Errorf("store: taking rate-limit lock: %w", err)
	}

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM help_requests WHERE user_id = $1 AND created_at >= $2`,
		in.UserID, since,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("store: counting requests since %s: %w", since, err)
	}
	if count >= limit {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO help_requests (id, user_id, problem_id, platform, n_submissions_taken, status)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		in.ID, in.UserID, in.ProblemID, in.Platform, in.NSubmissionsTaken, StatusPending,
	); err != nil {
		if isUniqueViolation(err) {
			return false, fmt.Errorf("%w: id %s", ErrDuplicateRequest, in.ID)
		}
		return false, fmt.Errorf("store: creating help request within daily limit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: committing help request: %w", err)
	}
	return true, nil
}

// GetHelpRequest fetches a help_requests row by id.
func (s *Store) GetHelpRequest(ctx context.Context, id uuid.UUID) (*HelpRequest, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, user_id, problem_id, platform, n_submissions_taken, status,
		       failure_reason, best_submission_id, hint_id, useless, error,
		       claimed_by, heartbeat_at, resume_step, repair_code, repair_run_id,
		       created_at, updated_at
		FROM help_requests WHERE id = $1`, id)

	var hr HelpRequest
	var status string
	if err := row.Scan(
		&hr.ID, &hr.UserID, &hr.ProblemID, &hr.Platform, &hr.NSubmissionsTaken, &status,
		&hr.FailureReason, &hr.BestSubmissionID, &hr.HintID, &hr.Useless, &hr.Error,
		&hr.ClaimedBy, &hr.HeartbeatAt, &hr.ResumeStep, &hr.RepairCode, &hr.RepairRunID,
		&hr.CreatedAt, &hr.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: id %s", ErrUnknownRequest, id)
		}
		return nil, fmt.Errorf("store: getting help request: %w", err)
	}
	hr.Status = Status(status)
	return &hr, nil
}

// TransitionStatus moves a help_requests row to a new status, validating the
// transition against the graph in the plan's Technical Details. Illegal
// transitions (including any transition out of a terminal status) return
// *ErrIllegalTransition and do not modify the row.
//
// The check and the write are one statement on purpose. A Store bound to
// the pool (production) runs each call in its own implicit transaction, so a
// separate SELECT ... FOR UPDATE would release its lock immediately and the
// pair would not be atomic — two workers racing over a reclaimed row could
// both read a legal source status and both write.
// workerID scopes the write to the row's current claimant, for the same
// reason Heartbeat does: a worker whose heartbeats lapsed long enough to be
// reclaimed only learns it lost the claim on its next heartbeat tick, and in
// that window it can finish its pipeline and write a terminal status onto a
// row the new claimant is actively processing. The transition itself is
// legal, so nothing else would catch it — the old worker's hint gets
// delivered and the healthy claimant fails on a row it no longer owns. An
// empty workerID skips the check, for callers with no claim of their own.
func (s *Store) TransitionStatus(ctx context.Context, id uuid.UUID, to Status, workerID string) error {
	row := s.db.QueryRow(ctx, `
		WITH upd AS (
			UPDATE help_requests SET status = $2, updated_at = now()
			WHERE id = $1 AND status = ANY($3)
			  AND ($4 = '' OR claimed_by IS NULL OR claimed_by = $4)
			RETURNING id
		)
		SELECT h.status, h.claimed_by, EXISTS (SELECT 1 FROM upd)
		FROM help_requests h WHERE h.id = $1`,
		id, to, transitionSources(to), workerID)

	var current string
	var claimedBy *string
	var updated bool
	if err := row.Scan(&current, &claimedBy, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, id)
		}
		return fmt.Errorf("store: updating status: %w", err)
	}
	if !updated && workerID != "" && claimedBy != nil && *claimedBy != workerID {
		return fmt.Errorf("%w: id %s is claimed by %s", ErrClaimLost, id, *claimedBy)
	}
	if !updated {
		// The outer SELECT sees the pre-UPDATE snapshot, so current is the
		// status the transition was rejected from.
		return &ErrIllegalTransition{From: Status(current), To: to}
	}
	return nil
}

// transitionSources returns every status that may legally transition to to,
// as the SQL predicate for the atomic update above.
func transitionSources(to Status) []string {
	var from []string
	for src, allowed := range legalTransitions {
		for _, dst := range allowed {
			if dst == to {
				from = append(from, string(src))
				break
			}
		}
	}
	return from
}

// ClaimNext atomically claims one pending help_requests row for workerID,
// using SELECT ... FOR UPDATE SKIP LOCKED so concurrent claimants each land
// on a distinct row instead of blocking on one another. Returns nil, nil if
// no row is pending — a miss, not an error.
func (s *Store) ClaimNext(ctx context.Context, workerID string) (*HelpRequest, error) {
	row := s.db.QueryRow(ctx, `
		UPDATE help_requests
		SET status = $1, claimed_by = $2, heartbeat_at = now(), updated_at = now(),
		    claim_attempts = claim_attempts + 1
		WHERE id = (
			SELECT id FROM help_requests
			WHERE status = $3
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, user_id, problem_id, platform, n_submissions_taken, status,
		          failure_reason, best_submission_id, hint_id, useless, error,
		          claimed_by, heartbeat_at, resume_step, repair_code, repair_run_id,
		          created_at, updated_at`,
		StatusRunning, workerID, StatusPending)

	var hr HelpRequest
	var status string
	// repair_code / repair_run_id are returned here too, not just by
	// GetHelpRequest: a claimed row that omitted them always read back
	// RepairCode == nil, so any caller trusting the claimed row directly
	// would see a resumed request as "checkpoint past repair with no code"
	// and fail it as corrupt — or, worse, re-run loop 1.
	if err := row.Scan(
		&hr.ID, &hr.UserID, &hr.ProblemID, &hr.Platform, &hr.NSubmissionsTaken, &status,
		&hr.FailureReason, &hr.BestSubmissionID, &hr.HintID, &hr.Useless, &hr.Error,
		&hr.ClaimedBy, &hr.HeartbeatAt, &hr.ResumeStep, &hr.RepairCode, &hr.RepairRunID,
		&hr.CreatedAt, &hr.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: claiming next help request: %w", err)
	}
	hr.Status = Status(status)
	return &hr, nil
}

// Heartbeat refreshes heartbeat_at for a request workerID is still actively
// working, reporting whether the row was actually refreshed. False (not an
// error) means this worker no longer owns the row: it either finished
// between the last tick and this one, or went stale and was reclaimed by
// another worker.
//
// The claimed_by predicate is what makes that distinguishable. Without it a
// worker whose heartbeats lapsed long enough to be reclaimed would keep
// refreshing a row another worker now owns — both would run the same
// pipeline to completion, double-submitting to the judge under the system
// account and double-spending the model budget, with only the final status
// transition detecting the race.
func (s *Store) Heartbeat(ctx context.Context, id uuid.UUID, workerID string) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`UPDATE help_requests SET heartbeat_at = now()
		 WHERE id = $1 AND status = $2 AND claimed_by = $3`,
		id, StatusRunning, workerID,
	)
	if err != nil {
		return false, fmt.Errorf("store: updating heartbeat: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// maxClaimAttempts bounds how many times one request may be claimed before
// the queue gives up on it. Without a bound, a request that crashes the
// pipeline deterministically (a panic, a store error, a corrupt checkpoint)
// never reaches a terminal status, so every sweep hands it back out — and
// since the repair and hint loops are not resume-guarded, each cycle
// re-spends both model budgets and re-submits to the judge, forever.
const maxClaimAttempts = 5

// ReclaimStale moves running rows whose heartbeat is older than before back
// to pending, clearing claimed_by/heartbeat_at so another worker can claim
// them. resume_step is deliberately left untouched, so the row resumes at
// its last completed pipeline step instead of restarting. Rows already
// claimed maxClaimAttempts times are moved to failed instead of pending.
// Returns the reclaimed (not the abandoned) request ids.
// staleAfter is compared against the database's own clock rather than
// against a cutoff computed here. heartbeat_at is always written by now() on
// the server (ClaimNext, Heartbeat), so mixing in the app host's clock made
// reclaim depend on the drift between two machines: run fast enough and
// every live row looks stale on the first sweep — reclaimed out from under a
// worker that is still submitting to the judge under the shared system login
// — and run slow enough and nothing is ever reclaimed, so the
// claim_attempts backstop never engages either.
func (s *Store) ReclaimStale(ctx context.Context, staleAfter time.Duration) ([]uuid.UUID, error) {
	if _, err := s.db.Exec(ctx, `
		UPDATE help_requests
		SET status = $1, claimed_by = NULL, heartbeat_at = NULL, updated_at = now(),
		    error = COALESCE(error || '; ', '') ||
		            'worker: abandoned after ' || claim_attempts || ' claim attempts without a terminal status'
		WHERE status = $2 AND heartbeat_at < now() - $3::interval AND claim_attempts >= $4`,
		StatusFailed, StatusRunning, staleAfter, maxClaimAttempts,
	); err != nil {
		return nil, fmt.Errorf("store: abandoning exhausted requests: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		UPDATE help_requests
		SET status = $1, claimed_by = NULL, heartbeat_at = NULL, updated_at = now()
		WHERE status = $2 AND heartbeat_at < now() - $3::interval AND claim_attempts < $4
		RETURNING id`,
		StatusPending, StatusRunning, staleAfter, maxClaimAttempts,
	)
	if err != nil {
		return nil, fmt.Errorf("store: reclaiming stale requests: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning reclaimed request id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating reclaimed requests: %w", err)
	}
	return ids, nil
}

// claimScopedUpdate runs one help_requests UPDATE whose WHERE clause is
// completed with the same claim predicate TransitionStatus uses, and maps a
// no-op to ErrClaimLost or ErrUnknownRequest.
//
// Every row mutator the pipeline calls mid-run needs this, not just the
// terminal transition. A worker whose heartbeats lapsed long enough to be
// reclaimed only learns it lost the claim on its next heartbeat tick, and in
// that window it is still executing steps: an unscoped SetResumeStep walks
// the new claimant's checkpoint backwards (re-running loop 1 on the next
// resume — a second judge submission under the shared system login and both
// model budgets again), and an unscoped SetRepairResult puts the old worker's
// code under the new claimant's run id, so loop 2 explains code that run
// never held. TransitionStatus alone catches none of that: it only fires
// after all of it has committed.
//
// setSQL is the "UPDATE help_requests SET ..." head only — the WHERE clause
// carrying the claim predicate is appended here. Its placeholders must be
// numbered so that $1 is the row id and $2 is the worker id; value arguments
// start at $3.
func (s *Store) claimScopedUpdate(ctx context.Context, id uuid.UUID, workerID, what, setSQL string, args ...any) error {
	params := append([]any{id, workerID}, args...)
	row := s.db.QueryRow(ctx, fmt.Sprintf(`
		WITH upd AS (
			%s
			WHERE id = $1 AND ($2 = '' OR claimed_by IS NULL OR claimed_by = $2)
			RETURNING id
		)
		SELECT h.claimed_by, EXISTS (SELECT 1 FROM upd)
		FROM help_requests h WHERE h.id = $1`, setSQL), params...)

	var claimedBy *string
	var updated bool
	if err := row.Scan(&claimedBy, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, id)
		}
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if !updated {
		return fmt.Errorf("%w: id %s is claimed by %s", ErrClaimLost, id, derefOrNone(claimedBy))
	}
	return nil
}

func derefOrNone(s *string) string {
	if s == nil {
		return "nobody"
	}
	return *s
}

// SetResumeStep records the last pipeline step a request completed, so a
// crash-reclaimed row resumes there instead of restarting from step 1.
func (s *Store) SetResumeStep(ctx context.Context, id uuid.UUID, workerID, step string) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting resume step",
		`UPDATE help_requests SET resume_step = $3, updated_at = now()`, step)
}

// SetRepairResult records loop 1's verified fix — the working code and the
// judge run it was accepted on — before the "repair" checkpoint is written,
// so a reclaimed row can hand that code to the hint loop instead of running
// loop 1 (and submitting to the judge) a second time.
func (s *Store) SetRepairResult(ctx context.Context, id uuid.UUID, workerID, code, runID string) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting repair result",
		`UPDATE help_requests SET repair_code = $3, repair_run_id = $4, updated_at = now()`, code, runID)
}

// SetBestSubmission records which snapshotted submission the pipeline picked
// as best for this request.
func (s *Store) SetBestSubmission(ctx context.Context, id uuid.UUID, workerID string, submissionID uuid.UUID) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting best submission",
		`UPDATE help_requests SET best_submission_id = $3, updated_at = now()`, submissionID)
}

// SetHintID records which hints row was (or will be) delivered for this
// request.
func (s *Store) SetHintID(ctx context.Context, id uuid.UUID, workerID string, hintID uuid.UUID) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting hint id",
		`UPDATE help_requests SET hint_id = $3, updated_at = now()`, hintID)
}

// SetFailureReason records why a no_fix/no_hint request stopped short of
// delivering a hint (e.g. "max_retries", "cost_cap").
func (s *Store) SetFailureReason(ctx context.Context, id uuid.UUID, workerID, reason string) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting failure reason",
		`UPDATE help_requests SET failure_reason = $3, updated_at = now()`, reason)
}

// SetError records the error message for a request that ends status=failed
// (infrastructure/platform error, as opposed to a declined no_fix/no_hint).
func (s *Store) SetError(ctx context.Context, id uuid.UUID, workerID, message string) error {
	return s.claimScopedUpdate(ctx, id, workerID, "setting error",
		`UPDATE help_requests SET error = $3, updated_at = now()`, message)
}

// AppendEvent inserts one events row.
func (s *Store) AppendEvent(ctx context.Context, requestID uuid.UUID, kind string, payload []byte) error {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO events (id, request_id, kind, payload) VALUES ($1, $2, $3, $4)`,
		uuid.New(), requestID, kind, payload,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, requestID)
		}
		return fmt.Errorf("store: appending event: %w", err)
	}
	return nil
}

// Event is an events row, as read back for tests/analytics.
type Event struct {
	ID        uuid.UUID
	RequestID uuid.UUID
	Kind      string
	Payload   []byte
	CreatedAt time.Time
}

// ListEvents returns every events row for a request, oldest first.
func (s *Store) ListEvents(ctx context.Context, requestID uuid.UUID) ([]Event, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, request_id, kind, payload, created_at
		FROM events WHERE request_id = $1 ORDER BY created_at, id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("store: listing events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RequestID, &e.Kind, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating events: %w", err)
	}
	return out, nil
}

// LLMCall is one llm_calls row. Cost is a decimal literal (e.g. "0.003400")
// so precision is never lost through a float — it is cast to the numeric
// column with ::numeric.
type LLMCall struct {
	ID                uuid.UUID
	RequestID         uuid.UUID
	Agent             string
	Model             string
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	Cost              string
	LatencyMS         int
	Attempt           int
	Prompt            string
	Response          string
}

// InsertLLMCall inserts one llm_calls row.
func (s *Store) InsertLLMCall(ctx context.Context, c LLMCall) error {
	id := c.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO llm_calls (
			id, request_id, agent, model, input_tokens, cached_input_tokens,
			output_tokens, cost, latency_ms, attempt, prompt, response
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12)`,
		id, c.RequestID, c.Agent, c.Model, c.InputTokens, c.CachedInputTokens,
		c.OutputTokens, c.Cost, c.LatencyMS, c.Attempt, c.Prompt, c.Response,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, c.RequestID)
		}
		return fmt.Errorf("store: inserting llm call: %w", err)
	}
	return nil
}

// GetLLMCall fetches one llm_calls row, with cost read back as its exact
// decimal text (never a float).
func (s *Store) GetLLMCall(ctx context.Context, id uuid.UUID) (*LLMCall, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, agent, model, input_tokens, cached_input_tokens,
		       output_tokens, cost::text, latency_ms, attempt, prompt, response
		FROM llm_calls WHERE id = $1`, id)

	var c LLMCall
	if err := row.Scan(
		&c.ID, &c.RequestID, &c.Agent, &c.Model, &c.InputTokens, &c.CachedInputTokens,
		&c.OutputTokens, &c.Cost, &c.LatencyMS, &c.Attempt, &c.Prompt, &c.Response,
	); err != nil {
		return nil, fmt.Errorf("store: getting llm call: %w", err)
	}
	return &c, nil
}

// Submission is one snapshotted submission row.
type Submission struct {
	ID                   uuid.UUID
	RequestID            uuid.UUID
	PlatformSubmissionID string
	Code                 string
	Language             string
	TestsPassed          int
	TestsTotal           int
	SubmittedAt          time.Time
	IsBest               bool
}

// SnapshotSubmissions inserts the submissions pulled for one request.
//
// The whole batch is one batched round trip, and each insert is idempotent
// against the (request_id, platform_submission_id) unique index added in
// migration 0004. Both halves matter on the crash path: the pipeline
// checkpoints StepSubmissions only *after* this returns, so a worker that
// dies mid-snapshot is reclaimed and runs the step again. Row-at-a-time
// inserts with no constraint left the earlier rows committed and then
// duplicated every submission on the retry — with a second is_best=true and
// an inflated snapshot size in HintEffectivenessInputs — once per reclaim,
// up to maxClaimAttempts times.
func (s *Store) SnapshotSubmissions(ctx context.Context, requestID uuid.UUID, subs []Submission) error {
	if len(subs) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, sub := range subs {
		id := sub.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		batch.Queue(`
			INSERT INTO submissions (
				id, request_id, platform_submission_id, code, language,
				tests_passed, tests_total, submitted_at, is_best
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (request_id, platform_submission_id) DO UPDATE
			SET is_best = EXCLUDED.is_best`,
			id, requestID, sub.PlatformSubmissionID, sub.Code, sub.Language,
			sub.TestsPassed, sub.TestsTotal, sub.SubmittedAt, sub.IsBest,
		)
	}

	results := s.db.SendBatch(ctx, batch)
	var firstErr error
	for range subs {
		if _, err := results.Exec(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		if isForeignKeyViolation(firstErr) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, requestID)
		}
		return fmt.Errorf("store: snapshotting submissions: %w", firstErr)
	}
	return nil
}

// ListSubmissions returns every snapshotted submission for a request.
func (s *Store) ListSubmissions(ctx context.Context, requestID uuid.UUID) ([]Submission, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, request_id, platform_submission_id, code, language,
		       tests_passed, tests_total, submitted_at, is_best
		FROM submissions WHERE request_id = $1 ORDER BY submitted_at`, requestID)
	if err != nil {
		return nil, fmt.Errorf("store: listing submissions: %w", err)
	}
	defer rows.Close()

	var out []Submission
	for rows.Next() {
		var sub Submission
		if err := rows.Scan(
			&sub.ID, &sub.RequestID, &sub.PlatformSubmissionID, &sub.Code, &sub.Language,
			&sub.TestsPassed, &sub.TestsTotal, &sub.SubmittedAt, &sub.IsBest,
		); err != nil {
			return nil, fmt.Errorf("store: scanning submission: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating submissions: %w", err)
	}
	return out, nil
}

// GetSubmission fetches one submissions row by id — used by a
// crash-resumed pipeline to reload the best submission it already
// snapshotted and picked before its last checkpoint.
func (s *Store) GetSubmission(ctx context.Context, id uuid.UUID) (*Submission, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, platform_submission_id, code, language,
		       tests_passed, tests_total, submitted_at, is_best
		FROM submissions WHERE id = $1`, id)

	var sub Submission
	if err := row.Scan(
		&sub.ID, &sub.RequestID, &sub.PlatformSubmissionID, &sub.Code, &sub.Language,
		&sub.TestsPassed, &sub.TestsTotal, &sub.SubmittedAt, &sub.IsBest,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: getting submission: no row with id %s", id)
		}
		return nil, fmt.Errorf("store: getting submission: %w", err)
	}
	return &sub, nil
}

// ShieldRecord is one shield_records row: the audit trail for what the
// shield stripped from a submission before any model saw it. CodeBefore,
// CodeAfter and Diff are all stored deliberately, for audit.
type ShieldRecord struct {
	ID         uuid.UUID
	RequestID  uuid.UUID
	CodeBefore string
	CodeAfter  string
	Diff       string
	Removed    []byte // jsonb: comments, unicode, counts
}

// InsertShieldRecord inserts one shield_records row.
func (s *Store) InsertShieldRecord(ctx context.Context, r ShieldRecord) error {
	id := r.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	removed := r.Removed
	if len(removed) == 0 {
		removed = []byte("{}")
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO shield_records (id, request_id, code_before, code_after, diff, removed)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, r.RequestID, r.CodeBefore, r.CodeAfter, r.Diff, removed,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, r.RequestID)
		}
		return fmt.Errorf("store: inserting shield record: %w", err)
	}
	return nil
}

// GetShieldRecord fetches one shield_records row by id.
func (s *Store) GetShieldRecord(ctx context.Context, id uuid.UUID) (*ShieldRecord, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, code_before, code_after, diff, removed
		FROM shield_records WHERE id = $1`, id)

	var r ShieldRecord
	if err := row.Scan(&r.ID, &r.RequestID, &r.CodeBefore, &r.CodeAfter, &r.Diff, &r.Removed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: getting shield record: no row with id %s", id)
		}
		return nil, fmt.Errorf("store: getting shield record: %w", err)
	}
	return &r, nil
}

// GetShieldRecordByRequest fetches the shield_records row for a request —
// used by a crash-resumed pipeline that already shielded the code before
// its last checkpoint, so it never re-shields (and re-inserts) it.
func (s *Store) GetShieldRecordByRequest(ctx context.Context, requestID uuid.UUID) (*ShieldRecord, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, code_before, code_after, diff, removed
		FROM shield_records WHERE request_id = $1
		ORDER BY created_at DESC LIMIT 1`, requestID)

	var r ShieldRecord
	if err := row.Scan(&r.ID, &r.RequestID, &r.CodeBefore, &r.CodeAfter, &r.Diff, &r.Removed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: getting shield record: no row for request %s", requestID)
		}
		return nil, fmt.Errorf("store: getting shield record: %w", err)
	}
	return &r, nil
}

// RawMistake is one raw_mistakes row: an unprocessed observation from a
// repair-loop attempt, later folded into a user's mistakes tally by the
// curator (Task 16).
type RawMistake struct {
	ID        uuid.UUID
	RequestID uuid.UUID
	UserID    string
	Text      string
	Processed bool
	CreatedAt time.Time
}

// InsertRawMistake inserts one raw_mistakes row with processed=false.
func (s *Store) InsertRawMistake(ctx context.Context, m RawMistake) error {
	id := m.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO raw_mistakes (id, request_id, user_id, text)
		VALUES ($1, $2, $3, $4)`,
		id, m.RequestID, m.UserID, m.Text,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, m.RequestID)
		}
		return fmt.Errorf("store: inserting raw mistake: %w", err)
	}
	return nil
}

// ListRawMistakes returns every raw_mistakes row for a request, oldest first.
func (s *Store) ListRawMistakes(ctx context.Context, requestID uuid.UUID) ([]RawMistake, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, request_id, user_id, text, processed, created_at
		FROM raw_mistakes WHERE request_id = $1 ORDER BY created_at, id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("store: listing raw mistakes: %w", err)
	}
	defer rows.Close()

	var out []RawMistake
	for rows.Next() {
		var m RawMistake
		if err := rows.Scan(&m.ID, &m.RequestID, &m.UserID, &m.Text, &m.Processed, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning raw mistake: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating raw mistakes: %w", err)
	}
	return out, nil
}

// ErrUnknownMistake is returned when an operation references a mistakes id
// that has no row.
var ErrUnknownMistake = errors.New("store: unknown mistake")

// ListUnprocessedRawMistakes returns every raw_mistakes row for a user that
// the curator (Task 16) has not yet folded into mistakes, oldest first —
// the batch one curator Run call processes together.
func (s *Store) ListUnprocessedRawMistakes(ctx context.Context, userID string) ([]RawMistake, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, request_id, user_id, text, processed, created_at
		FROM raw_mistakes WHERE user_id = $1 AND NOT processed ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing unprocessed raw mistakes: %w", err)
	}
	defer rows.Close()

	var out []RawMistake
	for rows.Next() {
		var m RawMistake
		if err := rows.Scan(&m.ID, &m.RequestID, &m.UserID, &m.Text, &m.Processed, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning unprocessed raw mistake: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating unprocessed raw mistakes: %w", err)
	}
	return out, nil
}

// MarkRawMistakesProcessed marks every raw_mistakes row in ids as
// processed=true — called once the curator's finish tool ends a user's
// batch, so those rows are never resent on a later sweep.
func (s *Store) MarkRawMistakesProcessed(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, `UPDATE raw_mistakes SET processed = TRUE WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: marking raw mistakes processed: %w", err)
	}
	return nil
}

// ListUsersWithUnprocessedMistakes returns every distinct user_id with at
// least one unprocessed raw_mistakes row — the nightly metaloop's worklist.
func (s *Store) ListUsersWithUnprocessedMistakes(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT user_id FROM raw_mistakes WHERE NOT processed ORDER BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing users with unprocessed mistakes: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("store: scanning user id: %w", err)
		}
		out = append(out, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating users with unprocessed mistakes: %w", err)
	}
	return out, nil
}

// Mistake is one mistakes row: a curated, per-user, per-habit tally the
// curator (Task 16) folds raw_mistakes into. Top-N by Count desc, LastSeen
// desc feeds the repair agent's prompt (Task 8).
type Mistake struct {
	ID          uuid.UUID
	UserID      string
	Title       string
	Description string
	Count       int
	FirstSeen   time.Time
	LastSeen    time.Time
}

// CreateMistake inserts a new mistakes row with count=1 — the curator's
// create_mistake tool, called for a genuinely new habit.
//
// first_seen/last_seen are set explicitly from clock_timestamp() rather
// than left to the column default (now()): now() is frozen to transaction
// start, so within one multi-statement test transaction every default
// would read identically and the top-N "count desc, last_seen desc"
// ordering (and MergeMistake's bump, below) could never be observed to
// move forward. clock_timestamp() advances on every call, transaction or
// not, matching what "last_seen" needs to mean.
func (s *Store) CreateMistake(ctx context.Context, m Mistake) error {
	id := m.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO mistakes (id, user_id, title, description, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, clock_timestamp(), clock_timestamp())`,
		id, m.UserID, m.Title, m.Description,
	)
	if err != nil {
		return fmt.Errorf("store: creating mistake: %w", err)
	}
	return nil
}

// MergeMistake increments an existing mistakes row's count and bumps
// last_seen to the current time — the curator's merge_into tool, called
// when a raw mistake is judged the same underlying habit as one already on
// file. See CreateMistake for why this uses clock_timestamp(), not now().
// The user_id predicate is not redundant: the id comes from a model that
// can hallucinate a well-formed uuid, and without it a curator sweep for one
// student could bump a different student's tally.
func (s *Store) MergeMistake(ctx context.Context, userID string, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE mistakes SET count = count + 1, last_seen = clock_timestamp() WHERE id = $1 AND user_id = $2`,
		id, userID)
	if err != nil {
		return fmt.Errorf("store: merging mistake: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %s", ErrUnknownMistake, id)
	}
	return nil
}

// ListMistakes returns every mistakes row for a user, most recently seen
// first — the curator's "already remembered" context.
func (s *Store) ListMistakes(ctx context.Context, userID string) ([]Mistake, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, user_id, title, description, count, first_seen, last_seen
		FROM mistakes WHERE user_id = $1 ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing mistakes: %w", err)
	}
	defer rows.Close()

	var out []Mistake
	for rows.Next() {
		var m Mistake
		if err := rows.Scan(&m.ID, &m.UserID, &m.Title, &m.Description, &m.Count, &m.FirstSeen, &m.LastSeen); err != nil {
			return nil, fmt.Errorf("store: scanning mistake: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating mistakes: %w", err)
	}
	return out, nil
}

// TopMistakes returns a user's mistakes ordered by count desc, then
// last_seen desc, limited to limit rows — what the repair prompt (Task 8)
// consumes as "this student's top-N recurring mistakes".
func (s *Store) TopMistakes(ctx context.Context, userID string, limit int) ([]Mistake, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, user_id, title, description, count, first_seen, last_seen
		FROM mistakes WHERE user_id = $1
		ORDER BY count DESC, last_seen DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing top mistakes: %w", err)
	}
	defer rows.Close()

	var out []Mistake
	for rows.Next() {
		var m Mistake
		if err := rows.Scan(&m.ID, &m.UserID, &m.Title, &m.Description, &m.Count, &m.FirstSeen, &m.LastSeen); err != nil {
			return nil, fmt.Errorf("store: scanning top mistake: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating top mistakes: %w", err)
	}
	return out, nil
}

// Hint is one hints row. The cache is deliberately cross-user: (ProblemID,
// CodeHash) identifies a defect, not a student, so an approved hint can be
// re-delivered to any request that hashes to the same post-shield code.
type Hint struct {
	ID        uuid.UUID
	RequestID uuid.UUID
	ProblemID string
	CodeHash  string
	Text      string
	Approved  bool
	CreatedAt time.Time
}

// InsertHint inserts one hints row.
func (s *Store) InsertHint(ctx context.Context, h Hint) error {
	id := h.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO hints (id, request_id, problem_id, code_hash, text, approved)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, h.RequestID, h.ProblemID, h.CodeHash, h.Text, h.Approved,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: id %s", ErrUnknownRequest, h.RequestID)
		}
		return fmt.Errorf("store: inserting hint: %w", err)
	}
	return nil
}

// GetHint fetches one hints row by id.
func (s *Store) GetHint(ctx context.Context, id uuid.UUID) (*Hint, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, problem_id, code_hash, text, approved, created_at
		FROM hints WHERE id = $1`, id)

	var h Hint
	if err := row.Scan(&h.ID, &h.RequestID, &h.ProblemID, &h.CodeHash, &h.Text, &h.Approved, &h.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: getting hint: no row with id %s", id)
		}
		return nil, fmt.Errorf("store: getting hint: %w", err)
	}
	return &h, nil
}

// FindApprovedHint returns the approved hint cached for this problem +
// post-shield code hash, or nil if none exists — a miss is not an error.
func (s *Store) FindApprovedHint(ctx context.Context, problemID, codeHash string) (*Hint, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, request_id, problem_id, code_hash, text, approved, created_at
		FROM hints
		WHERE problem_id = $1 AND code_hash = $2 AND approved
		ORDER BY created_at DESC
		LIMIT 1`, problemID, codeHash)

	var h Hint
	if err := row.Scan(&h.ID, &h.RequestID, &h.ProblemID, &h.CodeHash, &h.Text, &h.Approved, &h.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: finding approved hint: %w", err)
	}
	return &h, nil
}

// CountRequestsSince counts help_requests rows created by a user at or after
// since — the rate-limit query behind the API's daily_requests_per_user cap.
func (s *Store) CountRequestsSince(ctx context.Context, userID string, since time.Time) (int, error) {
	row := s.db.QueryRow(ctx,
		`SELECT count(*) FROM help_requests WHERE user_id = $1 AND created_at >= $2`,
		userID, since,
	)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting requests since %s: %w", since, err)
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
