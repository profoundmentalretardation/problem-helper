// Test isolation: no database. Handlers are tested against fakeStore, a
// scriptable in-memory fake of api.Store (same "unscripted call panics"
// pattern as internal/platform/mock) — Task 13/14 wire the real *store.Store
// and internal/worker into the same interfaces.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/api"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

type fakeStore struct {
	requests map[uuid.UUID]*store.HelpRequest
	hints    map[uuid.UUID]*store.Hint
	counts   map[string]int

	created []store.HelpRequestInput
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		requests: map[uuid.UUID]*store.HelpRequest{},
		hints:    map[uuid.UUID]*store.Hint{},
		counts:   map[string]int{},
	}
}

func (f *fakeStore) CreateHelpRequest(_ context.Context, in store.HelpRequestInput) error {
	f.created = append(f.created, in)
	f.requests[in.ID] = &store.HelpRequest{
		ID:                in.ID,
		UserID:            in.UserID,
		ProblemID:         in.ProblemID,
		Platform:          in.Platform,
		NSubmissionsTaken: in.NSubmissionsTaken,
		Status:            store.StatusPending,
	}
	return nil
}

func (f *fakeStore) GetHelpRequest(_ context.Context, id uuid.UUID) (*store.HelpRequest, error) {
	hr, ok := f.requests[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %s", store.ErrUnknownRequest, id)
	}
	return hr, nil
}

func (f *fakeStore) GetHint(_ context.Context, id uuid.UUID) (*store.Hint, error) {
	h, ok := f.hints[id]
	if !ok {
		panic(fmt.Sprintf("fakeStore: unscripted GetHint(%s)", id))
	}
	return h, nil
}

func (f *fakeStore) CountRequestsSince(_ context.Context, userID string, _ time.Time) (int, error) {
	return f.counts[userID], nil
}

func testConfig() *config.Config {
	return &config.Config{
		Env: config.Env{
			Platform:   "ejudge",
			APIToken:   "api-secret",
			AdminToken: "admin-secret",
		},
		Agents: config.AgentsConfig{
			Defaults: config.DefaultsConfig{
				NSubmissions:         25,
				DailyRequestsPerUser: 20,
			},
		},
	}
}

func newTestServer(t *testing.T, fs *fakeStore) http.Handler {
	t.Helper()
	return api.NewServer(fs, testConfig()).Handler()
}

func doRequest(h http.Handler, method, target, token string, body any) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandleHelp_CreatedWithDefaultNSubmissions(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/help", "api-secret", map[string]any{
		"user_id":    "alice",
		"problem_id": "p1",
	})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID == "" {
		t.Fatal("expected non-empty request_id")
	}
	id, err := uuid.Parse(resp.RequestID)
	if err != nil {
		t.Fatalf("request_id not a uuid: %v", err)
	}
	if len(fs.created) != 1 {
		t.Fatalf("expected 1 CreateHelpRequest call, got %d", len(fs.created))
	}
	got := fs.created[0]
	if got.ID != id || got.UserID != "alice" || got.ProblemID != "p1" || got.Platform != "ejudge" {
		t.Fatalf("unexpected CreateHelpRequest input: %+v", got)
	}
	if got.NSubmissionsTaken != 25 {
		t.Fatalf("expected default n_submissions 25, got %d", got.NSubmissionsTaken)
	}
}

func TestHandleHelp_ExplicitNSubmissions(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/help", "api-secret", map[string]any{
		"user_id":       "alice",
		"problem_id":    "p1",
		"n_submissions": 5,
	})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fs.created[0].NSubmissionsTaken != 5 {
		t.Fatalf("expected explicit n_submissions 5, got %d", fs.created[0].NSubmissionsTaken)
	}
}

func TestHandleHelp_Validation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing user_id", map[string]any{"problem_id": "p1"}},
		{"empty user_id", map[string]any{"user_id": "", "problem_id": "p1"}},
		{"missing problem_id", map[string]any{"user_id": "alice"}},
		{"zero n_submissions", map[string]any{"user_id": "alice", "problem_id": "p1", "n_submissions": 0}},
		{"negative n_submissions", map[string]any{"user_id": "alice", "problem_id": "p1", "n_submissions": -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			h := newTestServer(t, fs)
			w := doRequest(h, http.MethodPost, "/help", "api-secret", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			if len(fs.created) != 0 {
				t.Fatal("expected no help request created on validation failure")
			}
		})
	}
}

func TestHandleHelp_InvalidJSON(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)
	r := httptest.NewRequest(http.MethodPost, "/help", bytes.NewReader([]byte("not json")))
	r.Header.Set("Authorization", "Bearer api-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleHelp_RateLimited(t *testing.T) {
	fs := newFakeStore()
	fs.counts["alice"] = 20
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/help", "api-secret", map[string]any{
		"user_id":    "alice",
		"problem_id": "p1",
	})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body = %s", w.Code, w.Body.String())
	}
	if len(fs.created) != 0 {
		t.Fatal("expected no help request created when rate limited")
	}
}

func TestHandleHelp_UnderRateLimitStillWorks(t *testing.T) {
	fs := newFakeStore()
	fs.counts["alice"] = 19
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/help", "api-secret", map[string]any{
		"user_id":    "alice",
		"problem_id": "p1",
	})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleHelp_Auth(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)
	body := map[string]any{"user_id": "alice", "problem_id": "p1"}

	if w := doRequest(h, http.MethodPost, "/help", "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodPost, "/help", "wrong-token", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", w.Code)
	}
	if len(fs.created) != 0 {
		t.Fatal("expected no help request created on auth failure")
	}
}

func TestHandleGetRequest_PerStatusPayloads(t *testing.T) {
	hintID := uuid.New()

	cases := []struct {
		name  string
		hr    store.HelpRequest
		hint  *store.Hint
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "already_solved",
			hr:   store.HelpRequest{Status: store.StatusAlreadySolved},
			check: func(t *testing.T, body map[string]any) {
				if body["message"] != "problem already solved, nothing to do" {
					t.Fatalf("unexpected message: %v", body["message"])
				}
			},
		},
		{
			name: "no_submissions",
			hr:   store.HelpRequest{Status: store.StatusNoSubmissions},
			check: func(t *testing.T, body map[string]any) {
				if body["message"] != "no submissions to analyze yet" {
					t.Fatalf("unexpected message: %v", body["message"])
				}
			},
		},
		{
			name: "done",
			hr:   store.HelpRequest{Status: store.StatusDone, HintID: &hintID},
			hint: &store.Hint{ID: hintID, Text: "try checking your loop bounds"},
			check: func(t *testing.T, body map[string]any) {
				if body["hint"] != "try checking your loop bounds" {
					t.Fatalf("unexpected hint: %v", body["hint"])
				}
			},
		},
		{
			name: "no_fix",
			hr:   store.HelpRequest{Status: store.StatusNoFix},
			check: func(t *testing.T, body map[string]any) {
				if body["message"] != "repair loop exhausted retries/cost; no working code found" {
					t.Fatalf("unexpected message: %v", body["message"])
				}
			},
		},
		{
			name: "no_hint",
			hr:   store.HelpRequest{Status: store.StatusNoHint},
			check: func(t *testing.T, body map[string]any) {
				if body["message"] != "working code found but no hint passed the guardrail" {
					t.Fatalf("unexpected message: %v", body["message"])
				}
			},
		},
		{
			name: "failed",
			hr:   store.HelpRequest{Status: store.StatusFailed, Error: strPtr("platform unreachable")},
			check: func(t *testing.T, body map[string]any) {
				if body["error"] != "platform unreachable" {
					t.Fatalf("unexpected error: %v", body["error"])
				}
			},
		},
		{
			name: "pending",
			hr:   store.HelpRequest{Status: store.StatusPending},
			check: func(t *testing.T, body map[string]any) {
				if _, ok := body["message"]; ok {
					t.Fatalf("expected no message for pending, got %v", body["message"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			id := uuid.New()
			hr := tc.hr
			hr.ID = id
			fs.requests[id] = &hr
			if tc.hint != nil {
				fs.hints[tc.hint.ID] = tc.hint
			}
			h := newTestServer(t, fs)

			w := doRequest(h, http.MethodGet, "/requests/"+id.String(), "api-secret", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["status"] != string(tc.hr.Status) {
				t.Fatalf("status field = %v, want %v", body["status"], tc.hr.Status)
			}
			tc.check(t, body)
		})
	}
}

func TestHandleGetRequest_UnknownID(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)
	w := doRequest(h, http.MethodGet, "/requests/"+uuid.New().String(), "api-secret", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleGetRequest_InvalidID(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)
	w := doRequest(h, http.MethodGet, "/requests/not-a-uuid", "api-secret", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleGetRequest_Auth(t *testing.T) {
	fs := newFakeStore()
	id := uuid.New()
	fs.requests[id] = &store.HelpRequest{ID: id, Status: store.StatusPending}
	h := newTestServer(t, fs)

	if w := doRequest(h, http.MethodGet, "/requests/"+id.String(), "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodGet, "/requests/"+id.String(), "wrong-token", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", w.Code)
	}
}

func TestHandleAdmin_Auth(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	if w := doRequest(h, http.MethodGet, "/admin/anything", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodGet, "/admin/anything", "wrong-token", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", w.Code)
	}
	// API_TOKEN must not authorize admin routes.
	if w := doRequest(h, http.MethodGet, "/admin/anything", "api-secret", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("api token on admin route: status = %d, want 401", w.Code)
	}
	// Correct admin token passes auth; no route is implemented yet (Task 17).
	if w := doRequest(h, http.MethodGet, "/admin/anything", "admin-secret", nil); w.Code == http.StatusUnauthorized {
		t.Fatalf("admin token: status = %d, should not be 401", w.Code)
	}
}

func strPtr(s string) *string { return &s }
