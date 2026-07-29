package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CostByRequest sums llm_calls.cost for one request, as an exact decimal
// string; "0" if the request has no calls yet.
func (s *Store) CostByRequest(ctx context.Context, requestID uuid.UUID) (string, error) {
	row := s.db.QueryRow(ctx,
		`SELECT COALESCE(SUM(cost), 0)::text FROM llm_calls WHERE request_id = $1`, requestID)
	var cost string
	if err := row.Scan(&cost); err != nil {
		return "", fmt.Errorf("store: summing cost for request %s: %w", requestID, err)
	}
	return cost, nil
}

// ModelCost is one model's total spend across every llm_calls row.
type ModelCost struct {
	Model string
	Cost  string
}

// CostByModel sums llm_calls.cost grouped by model, across all requests.
func (s *Store) CostByModel(ctx context.Context) ([]ModelCost, error) {
	rows, err := s.db.Query(ctx,
		`SELECT model, SUM(cost)::text FROM llm_calls GROUP BY model ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("store: summing cost by model: %w", err)
	}
	defer rows.Close()

	var out []ModelCost
	for rows.Next() {
		var mc ModelCost
		if err := rows.Scan(&mc.Model, &mc.Cost); err != nil {
			return nil, fmt.Errorf("store: scanning cost by model: %w", err)
		}
		out = append(out, mc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating cost by model: %w", err)
	}
	return out, nil
}

// AgentCost is one agent's total spend across every llm_calls row.
type AgentCost struct {
	Agent string
	Cost  string
}

// CostByAgent sums llm_calls.cost grouped by agent, across all requests.
func (s *Store) CostByAgent(ctx context.Context) ([]AgentCost, error) {
	rows, err := s.db.Query(ctx,
		`SELECT agent, SUM(cost)::text FROM llm_calls GROUP BY agent ORDER BY agent`)
	if err != nil {
		return nil, fmt.Errorf("store: summing cost by agent: %w", err)
	}
	defer rows.Close()

	var out []AgentCost
	for rows.Next() {
		var ac AgentCost
		if err := rows.Scan(&ac.Agent, &ac.Cost); err != nil {
			return nil, fmt.Errorf("store: scanning cost by agent: %w", err)
		}
		out = append(out, ac)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating cost by agent: %w", err)
	}
	return out, nil
}

// RequestCountsByStatus counts help_requests rows grouped by status —
// separates "our infra broke" (failed) from "we declined to give a hint"
// (no_fix / no_hint) for analytics, per the plan's Technical Details.
// Statuses with zero rows are simply absent from the map.
func (s *Store) RequestCountsByStatus(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.Query(ctx, `SELECT status, count(*) FROM help_requests GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: counting requests by status: %w", err)
	}
	defer rows.Close()

	out := map[Status]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: scanning request count by status: %w", err)
		}
		out[Status(status)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating request counts by status: %w", err)
	}
	return out, nil
}

// HintEffectivenessRow is one help_requests row's contribution to computing
// hint effectiveness downstream: the raw submission-snapshot counts and
// hint-delivery timestamp needed to answer "how many submissions until
// solved after the hint", without this query trying to compute that itself
// (a later request's snapshot is what shows whether the student solved it).
type HintEffectivenessRow struct {
	RequestID       uuid.UUID
	CreatedAt       time.Time
	SubmissionCount int
	LastSubmittedAt *time.Time
	HintDeliveredAt *time.Time
}

// HintEffectivenessInputs returns, oldest first, one row per help_requests
// for userID+problemID: how many submissions were snapshotted, when the
// last one was submitted, and when (if ever) a hint was delivered for that
// request — the inputs a downstream analysis joins across requests to
// compute effectiveness.
func (s *Store) HintEffectivenessInputs(ctx context.Context, userID, problemID string) ([]HintEffectivenessRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT hr.id, hr.created_at,
		       -- DISTINCT because the events join below fans this row out
		       -- once per hint_delivered event, and a request can have more
		       -- than one (a cache re-delivery, or a redelivery after a
		       -- crash between the event and the status transition).
		       COUNT(DISTINCT sub.id) AS submission_count,
		       MAX(sub.submitted_at) AS last_submitted_at,
		       MAX(ev.created_at) FILTER (WHERE ev.kind = 'hint_delivered') AS hint_delivered_at
		FROM help_requests hr
		LEFT JOIN submissions sub ON sub.request_id = hr.id
		LEFT JOIN events ev ON ev.request_id = hr.id AND ev.kind = 'hint_delivered'
		WHERE hr.user_id = $1 AND hr.problem_id = $2
		GROUP BY hr.id
		ORDER BY hr.created_at`, userID, problemID)
	if err != nil {
		return nil, fmt.Errorf("store: reading hint effectiveness inputs: %w", err)
	}
	defer rows.Close()

	var out []HintEffectivenessRow
	for rows.Next() {
		var r HintEffectivenessRow
		if err := rows.Scan(&r.RequestID, &r.CreatedAt, &r.SubmissionCount, &r.LastSubmittedAt, &r.HintDeliveredAt); err != nil {
			return nil, fmt.Errorf("store: scanning hint effectiveness row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating hint effectiveness inputs: %w", err)
	}
	return out, nil
}

// SetUseless sets help_requests.useless — the admin flag marking a
// delivered hint as unhelpful, used to keep analytics on hint quality
// separate from the pipeline's own success/failure statuses.
func (s *Store) SetUseless(ctx context.Context, id uuid.UUID, useless bool) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE help_requests SET useless = $1, updated_at = now() WHERE id = $2`, useless, id)
	if err != nil {
		return fmt.Errorf("store: setting useless: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %s", ErrUnknownRequest, id)
	}
	return nil
}

// RequestFilter is the set of optional filters ListRequests supports; a nil
// field means "don't filter on this".
type RequestFilter struct {
	Useless *bool
	Status  *Status
	Model   *string // matches requests with at least one llm_calls row using this model
}

// ListRequests returns help_requests rows matching filter, newest first —
// the admin request listing (Task 17).
func (s *Store) ListRequests(ctx context.Context, filter RequestFilter) ([]HelpRequest, error) {
	query := `
		SELECT DISTINCT hr.id, hr.user_id, hr.problem_id, hr.platform, hr.n_submissions_taken, hr.status,
		       hr.failure_reason, hr.best_submission_id, hr.hint_id, hr.useless, hr.error,
		       hr.claimed_by, hr.heartbeat_at, hr.resume_step, hr.created_at, hr.updated_at
		FROM help_requests hr`

	var conditions []string
	var args []any
	if filter.Model != nil {
		query += ` JOIN llm_calls lc ON lc.request_id = hr.id`
		args = append(args, *filter.Model)
		conditions = append(conditions, fmt.Sprintf("lc.model = $%d", len(args)))
	}
	if filter.Useless != nil {
		args = append(args, *filter.Useless)
		conditions = append(conditions, fmt.Sprintf("hr.useless = $%d", len(args)))
	}
	if filter.Status != nil {
		args = append(args, string(*filter.Status))
		conditions = append(conditions, fmt.Sprintf("hr.status = $%d", len(args)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY hr.created_at DESC"

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing requests: %w", err)
	}
	defer rows.Close()

	var out []HelpRequest
	for rows.Next() {
		var hr HelpRequest
		var status string
		if err := rows.Scan(
			&hr.ID, &hr.UserID, &hr.ProblemID, &hr.Platform, &hr.NSubmissionsTaken, &status,
			&hr.FailureReason, &hr.BestSubmissionID, &hr.HintID, &hr.Useless, &hr.Error,
			&hr.ClaimedBy, &hr.HeartbeatAt, &hr.ResumeStep, &hr.CreatedAt, &hr.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scanning listed request: %w", err)
		}
		hr.Status = Status(status)
		out = append(out, hr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating listed requests: %w", err)
	}
	return out, nil
}
