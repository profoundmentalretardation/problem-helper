// Package api is the HTTP layer: POST /help + GET /requests/{id} for the
// synchronous half of the pipeline (the worker, wired in Task 13/14, does
// the rest), bearer auth for both the caller-facing and admin routes, and
// the per-user daily rate limit. Handlers depend only on the narrow Store
// interface below, so tests run against a fake — no database needed.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// Store is the persistence dependency handlers need; *store.Store satisfies
// it.
type Store interface {
	CreateHelpRequestWithinDailyLimit(ctx context.Context, in store.HelpRequestInput, since time.Time, limit int) (bool, error)
	GetHelpRequest(ctx context.Context, id uuid.UUID) (*store.HelpRequest, error)
	GetHint(ctx context.Context, id uuid.UUID) (*store.Hint, error)
	SetUseless(ctx context.Context, id uuid.UUID, useless bool) error
	ListRequests(ctx context.Context, filter store.RequestFilter) ([]store.HelpRequest, error)
}

// Server holds everything the handlers need: the store, the two bearer
// tokens, the agents.yaml defaults for n_submissions and the daily
// per-user request cap, and the admin-triggered metaloop sweep.
type Server struct {
	store    Store
	metaloop worker.MetaloopRunner

	apiToken             string
	adminToken           string
	platform             string
	defaultNSubmissions  int
	dailyRequestsPerUser int

	now func() time.Time
}

// NewServer builds a Server from the loaded config. metaloop drives
// POST /admin/metaloop/run — *worker.Metaloop in production.
func NewServer(st Store, cfg *config.Config, metaloop worker.MetaloopRunner) *Server {
	return &Server{
		store:                st,
		metaloop:             metaloop,
		apiToken:             cfg.Env.APIToken,
		adminToken:           cfg.Env.AdminToken,
		platform:             cfg.Env.Platform,
		defaultNSubmissions:  cfg.Agents.Defaults.NSubmissions,
		dailyRequestsPerUser: cfg.Agents.Defaults.DailyRequestsPerUser,
		now:                  time.Now,
	}
}

// Handler builds the routed, auth-wrapped http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /help", s.withAuth(s.apiToken, s.handleHelp))
	mux.HandleFunc("GET /requests/{id}", s.withAuth(s.apiToken, s.handleGetRequest))
	mux.HandleFunc("POST /admin/metaloop/run", s.withAuth(s.adminToken, s.handleMetaloopRun))
	mux.HandleFunc("POST /admin/requests/{id}/useless", s.withAuth(s.adminToken, s.handleSetUseless))
	mux.HandleFunc("GET /admin/requests", s.withAuth(s.adminToken, s.handleListRequests))
	// catch-all still lets the ADMIN_TOKEN gate be exercised and enforced
	// for any admin path with no route above.
	mux.Handle("/admin/", s.withAuth(s.adminToken, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	return mux
}

// withAuth requires "Authorization: Bearer <token>" to match want exactly,
// comparing in constant time so token length/prefix mismatches can't be
// timed. A missing or wrong bearer is always 401.
func (s *Server) withAuth(want string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		got, ok := "", false
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			got, ok = auth[len(prefix):], true
		}
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

type helpRequestBody struct {
	UserID       string `json:"user_id"`
	ProblemID    string `json:"problem_id"`
	NSubmissions *int   `json:"n_submissions"`
}

// maxHelpBodyBytes caps POST /help's body. The endpoint takes two short
// identifiers and an integer; anything larger is not a request we serve.
const maxHelpBodyBytes = 8 << 10

// maxIdentifierLen bounds user_id and problem_id. Both are forwarded to the
// judging platform (ejudge interpolates user_id into a server-side filter
// expression) and stored in unbounded TEXT columns.
const maxIdentifierLen = 128

// maxNSubmissions caps how many submissions one request may pull, so a
// caller can't turn a single /help into an unbounded platform scrape.
const maxNSubmissions = 200

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	var body helpRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHelpBodyBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}
	if len(body.UserID) > maxIdentifierLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is too long"})
		return
	}
	if body.ProblemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "problem_id is required"})
		return
	}
	if len(body.ProblemID) > maxIdentifierLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "problem_id is too long"})
		return
	}
	nSubmissions := s.defaultNSubmissions
	if body.NSubmissions != nil {
		if *body.NSubmissions <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "n_submissions must be positive"})
			return
		}
		if *body.NSubmissions > maxNSubmissions {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "n_submissions is too large"})
			return
		}
		nSubmissions = *body.NSubmissions
	}

	ctx := r.Context()
	since := startOfDay(s.now())
	// The cap is enforced by the insert itself rather than by a count-then-
	// insert pair, so concurrent requests for one user cannot all observe the
	// same pre-insert count and all slip through. See
	// store.CreateHelpRequestWithinDailyLimit.
	id := uuid.New()
	created, err := s.store.CreateHelpRequestWithinDailyLimit(ctx, store.HelpRequestInput{
		ID:                id,
		UserID:            body.UserID,
		ProblemID:         body.ProblemID,
		Platform:          s.platform,
		NSubmissionsTaken: nSubmissions,
	}, since, s.dailyRequestsPerUser)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !created {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "daily request limit reached"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": id.String()})
}

// statusMessages holds the fixed text for terminal statuses that carry a
// plain explanation rather than a hint or an error, verbatim from the
// plan's Technical Details.
var statusMessages = map[store.Status]string{
	store.StatusAlreadySolved: "problem already solved, nothing to do",
	store.StatusNoSubmissions: "no submissions to analyze yet",
	store.StatusNoFix:         "repair loop exhausted retries/cost; no working code found",
	store.StatusNoHint:        "working code found but no hint passed the guardrail",
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request id"})
		return
	}

	ctx := r.Context()
	hr, err := s.store.GetHelpRequest(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRequest) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := map[string]any{
		"request_id": hr.ID.String(),
		"status":     string(hr.Status),
	}

	switch hr.Status {
	case store.StatusDone:
		if hr.HintID != nil {
			hint, err := s.store.GetHint(ctx, *hr.HintID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			resp["hint"] = hint.Text
		}
	case store.StatusFailed:
		// help_requests.error holds a wrapped Go error, which can carry the
		// LLM provider's raw response body, ejudge URLs, or DB error text.
		// Callers get a fixed message; the detail stays in the row and the
		// events log for operators.
		if hr.Error != nil {
			resp["error"] = "internal error while processing this request"
		}
	default:
		if msg, ok := statusMessages[hr.Status]; ok {
			resp["message"] = msg
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleMetaloopRun triggers an out-of-band curator sweep across every user
// with unprocessed raw mistakes — the same sweep the nightly cron runs
// (internal/worker), exposed for manual/testing use.
func (s *Server) handleMetaloopRun(w http.ResponseWriter, r *http.Request) {
	summary, err := s.metaloop.Run(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"users_processed": summary.UsersProcessed,
		"merged":          summary.Merged,
		"created":         summary.Created,
		"gave_up":         summary.GaveUp,
	})
}

// handleSetUseless marks a request's delivered hint as unhelpful — an
// admin judgment call kept separate from the pipeline's own status, so
// analytics can tell "our infra broke" / "we declined" / "we helped, but
// badly" apart.
func (s *Server) handleSetUseless(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request id"})
		return
	}

	if err := s.store.SetUseless(r.Context(), id, true); err != nil {
		if errors.Is(err, store.ErrUnknownRequest) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"request_id": id.String(), "useless": "true"})
}

// handleListRequests lists help_requests, optionally filtered by
// useless/status/model (model matches a request with at least one
// llm_calls row using it) — the admin request listing.
func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	filter := store.RequestFilter{}

	if v := r.URL.Query().Get("useless"); v != "" {
		useless, err := strconv.ParseBool(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid useless filter"})
			return
		}
		filter.Useless = &useless
	}
	if v := r.URL.Query().Get("status"); v != "" {
		status := store.Status(v)
		filter.Status = &status
	}
	if v := r.URL.Query().Get("model"); v != "" {
		filter.Model = &v
	}

	requests, err := s.store.ListRequests(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := make([]map[string]any, len(requests))
	for i, hr := range requests {
		resp[i] = map[string]any{
			"request_id": hr.ID.String(),
			"user_id":    hr.UserID,
			"problem_id": hr.ProblemID,
			"status":     string(hr.Status),
			"useless":    hr.Useless,
		}
		// The real failure detail, redacted from the caller-facing
		// GET /requests/{id}, stays available here — this route is behind
		// ADMIN_TOKEN, so operators keep an HTTP path to it.
		if hr.Error != nil {
			resp[i]["error"] = *hr.Error
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
