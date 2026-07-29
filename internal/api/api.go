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
	CreateHelpRequest(ctx context.Context, in store.HelpRequestInput) error
	GetHelpRequest(ctx context.Context, id uuid.UUID) (*store.HelpRequest, error)
	GetHint(ctx context.Context, id uuid.UUID) (*store.Hint, error)
	CountRequestsSince(ctx context.Context, userID string, since time.Time) (int, error)
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

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	var body helpRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}
	if body.ProblemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "problem_id is required"})
		return
	}
	nSubmissions := s.defaultNSubmissions
	if body.NSubmissions != nil {
		if *body.NSubmissions <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "n_submissions must be positive"})
			return
		}
		nSubmissions = *body.NSubmissions
	}

	ctx := r.Context()
	since := startOfDay(s.now())
	count, err := s.store.CountRequestsSince(ctx, body.UserID, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if count >= s.dailyRequestsPerUser {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "daily request limit reached"})
		return
	}

	id := uuid.New()
	if err := s.store.CreateHelpRequest(ctx, store.HelpRequestInput{
		ID:                id,
		UserID:            body.UserID,
		ProblemID:         body.ProblemID,
		Platform:          s.platform,
		NSubmissionsTaken: nSubmissions,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
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
		if hr.Error != nil {
			resp["error"] = *hr.Error
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
