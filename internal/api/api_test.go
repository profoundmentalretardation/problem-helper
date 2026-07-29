// Test isolation: no database. Handlers are tested against fakeStore, a
// scriptable in-memory fake of api.Store (same "unscripted call panics"
// pattern as internal/platform/mock) — Task 13/14 wire the real *store.Store
// and internal/worker into the same interfaces.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/api"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

type fakeStore struct {
	requests map[uuid.UUID]*store.HelpRequest
	hints    map[uuid.UUID]*store.Hint
	counts   map[string]int

	created []store.HelpRequestInput

	uselessCalls []uuid.UUID
	uselessErr   error

	listFilter store.RequestFilter
	listResult []store.HelpRequest
	listErr    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		requests: map[uuid.UUID]*store.HelpRequest{},
		hints:    map[uuid.UUID]*store.Hint{},
		counts:   map[string]int{},
	}
}

// CreateHelpRequestWithinDailyLimit mirrors the store's single-statement
// enforcement: over the limit means no row is written and created=false.
func (f *fakeStore) CreateHelpRequestWithinDailyLimit(_ context.Context, in store.HelpRequestInput, _ time.Time, limit int) (bool, error) {
	if f.counts[in.UserID] >= limit {
		return false, nil
	}
	f.counts[in.UserID]++
	f.created = append(f.created, in)
	f.requests[in.ID] = &store.HelpRequest{
		ID:                in.ID,
		UserID:            in.UserID,
		ProblemID:         in.ProblemID,
		Platform:          in.Platform,
		NSubmissionsTaken: in.NSubmissionsTaken,
		Status:            store.StatusPending,
	}
	return true, nil
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

func (f *fakeStore) SetUseless(_ context.Context, id uuid.UUID, useless bool) error {
	if f.uselessErr != nil {
		return f.uselessErr
	}
	f.uselessCalls = append(f.uselessCalls, id)
	hr, ok := f.requests[id]
	if !ok {
		return fmt.Errorf("%w: id %s", store.ErrUnknownRequest, id)
	}
	hr.Useless = useless
	return nil
}

func (f *fakeStore) ListRequests(_ context.Context, filter store.RequestFilter) ([]store.HelpRequest, error) {
	f.listFilter = filter
	return f.listResult, f.listErr
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

// fakeMetaloopRunner is a scriptable api.MetaloopRunner (worker.MetaloopRunner):
// returns a canned summary/error and counts calls.
type fakeMetaloopRunner struct {
	summary worker.MetaloopSummary
	err     error
	calls   int
}

func (f *fakeMetaloopRunner) Run(_ context.Context) (worker.MetaloopSummary, error) {
	f.calls++
	return f.summary, f.err
}

func newTestServer(t *testing.T, fs *fakeStore) http.Handler {
	t.Helper()
	return newTestServerWithMetaloop(t, fs, &fakeMetaloopRunner{})
}

func newTestServerWithMetaloop(t *testing.T, fs *fakeStore, ml worker.MetaloopRunner) http.Handler {
	t.Helper()
	return api.NewServer(fs, testConfig(), ml).Handler()
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
		{"n_submissions over cap", map[string]any{"user_id": "alice", "problem_id": "p1", "n_submissions": 201}},
		{"user_id too long", map[string]any{"user_id": strings.Repeat("a", 129), "problem_id": "p1"}},
		{"problem_id too long", map[string]any{"user_id": "alice", "problem_id": strings.Repeat("p", 129)}},
		{"body over cap", map[string]any{"user_id": "alice", "problem_id": "p1", "padding": strings.Repeat("x", 9<<10)}},
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

// TestHandleHelp_LimitBoundariesAreAccepted pins the accept side of the
// limits, so tightening one without meaning to fails here rather than
// silently rejecting legitimate callers.
func TestHandleHelp_LimitBoundariesAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"user_id at max length", map[string]any{"user_id": strings.Repeat("a", 128), "problem_id": "p1"}},
		{"problem_id at max length", map[string]any{"user_id": "alice", "problem_id": strings.Repeat("p", 128)}},
		{"n_submissions at cap", map[string]any{"user_id": "alice", "problem_id": "p1", "n_submissions": 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			h := newTestServer(t, fs)
			w := doRequest(h, http.MethodPost, "/help", "api-secret", tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202, body = %s", w.Code, w.Body.String())
			}
			if len(fs.created) != 1 {
				t.Fatalf("created %d help requests, want 1", len(fs.created))
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
			// help_requests.error carries wrapped Go errors — provider
			// response bodies, ejudge URLs, DB text. Callers get a fixed
			// message and none of that detail.
			hr: store.HelpRequest{Status: store.StatusFailed, Error: strPtr("pgx: dial tcp 10.0.0.7:5432: refused")},
			check: func(t *testing.T, body map[string]any) {
				if body["error"] != "internal error while processing this request" {
					t.Fatalf("unexpected error: %v", body["error"])
				}
				for _, leak := range []string{"pgx", "10.0.0.7", "5432"} {
					if strings.Contains(fmt.Sprint(body["error"]), leak) {
						t.Fatalf("internal detail %q leaked to the caller: %v", leak, body["error"])
					}
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
	// Correct admin token passes auth; this path has no route implemented
	// (Task 17 adds more admin endpoints under this prefix).
	if w := doRequest(h, http.MethodGet, "/admin/anything", "admin-secret", nil); w.Code == http.StatusUnauthorized {
		t.Fatalf("admin token: status = %d, should not be 401", w.Code)
	}
}

func TestHandleMetaloopRun_Auth(t *testing.T) {
	fs := newFakeStore()
	h := newTestServerWithMetaloop(t, fs, &fakeMetaloopRunner{})

	if w := doRequest(h, http.MethodPost, "/admin/metaloop/run", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodPost, "/admin/metaloop/run", "api-secret", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("api token: status = %d, want 401", w.Code)
	}
}

func TestHandleMetaloopRun_Success(t *testing.T) {
	fs := newFakeStore()
	ml := &fakeMetaloopRunner{summary: worker.MetaloopSummary{UsersProcessed: 2, Merged: 1, Created: 3, GaveUp: 1}}
	h := newTestServerWithMetaloop(t, fs, ml)

	w := doRequest(h, http.MethodPost, "/admin/metaloop/run", "admin-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ml.calls != 1 {
		t.Fatalf("metaloop calls = %d, want 1", ml.calls)
	}

	var resp map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["users_processed"] != 2 || resp["merged"] != 1 || resp["created"] != 3 || resp["gave_up"] != 1 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestHandleMetaloopRun_Error(t *testing.T) {
	fs := newFakeStore()
	ml := &fakeMetaloopRunner{err: errors.New("boom")}
	h := newTestServerWithMetaloop(t, fs, ml)

	w := doRequest(h, http.MethodPost, "/admin/metaloop/run", "admin-secret", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleSetUseless_Success(t *testing.T) {
	fs := newFakeStore()
	id := uuid.New()
	fs.requests[id] = &store.HelpRequest{ID: id, Status: store.StatusDone}
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/admin/requests/"+id.String()+"/useless", "admin-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(fs.uselessCalls) != 1 || fs.uselessCalls[0] != id {
		t.Fatalf("uselessCalls = %v, want [%s]", fs.uselessCalls, id)
	}
	if !fs.requests[id].Useless {
		t.Fatal("expected request to be marked useless")
	}
}

func TestHandleSetUseless_UnknownID(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/admin/requests/"+uuid.New().String()+"/useless", "admin-secret", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleSetUseless_InvalidID(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodPost, "/admin/requests/not-a-uuid/useless", "admin-secret", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetUseless_Auth(t *testing.T) {
	fs := newFakeStore()
	id := uuid.New()
	fs.requests[id] = &store.HelpRequest{ID: id, Status: store.StatusDone}
	h := newTestServer(t, fs)

	if w := doRequest(h, http.MethodPost, "/admin/requests/"+id.String()+"/useless", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodPost, "/admin/requests/"+id.String()+"/useless", "api-secret", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("api token: status = %d, want 401", w.Code)
	}
	if len(fs.uselessCalls) != 0 {
		t.Fatal("expected no SetUseless call on auth failure")
	}
}

func TestHandleListRequests_NoFilters(t *testing.T) {
	fs := newFakeStore()
	id := uuid.New()
	fs.listResult = []store.HelpRequest{{ID: id, UserID: "alice", ProblemID: "p1", Status: store.StatusDone}}
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodGet, "/admin/requests", "admin-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fs.listFilter.Useless != nil || fs.listFilter.Status != nil || fs.listFilter.Model != nil {
		t.Fatalf("expected empty filter, got %+v", fs.listFilter)
	}
	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 1 || resp[0]["request_id"] != id.String() {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHandleListRequests_Filters(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodGet, "/admin/requests?useless=true&status=no_fix&model=gpt-a", "admin-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fs.listFilter.Useless == nil || !*fs.listFilter.Useless {
		t.Fatalf("expected useless=true filter, got %+v", fs.listFilter.Useless)
	}
	if fs.listFilter.Status == nil || *fs.listFilter.Status != store.StatusNoFix {
		t.Fatalf("expected status=no_fix filter, got %+v", fs.listFilter.Status)
	}
	if fs.listFilter.Model == nil || *fs.listFilter.Model != "gpt-a" {
		t.Fatalf("expected model=gpt-a filter, got %+v", fs.listFilter.Model)
	}
}

func TestHandleListRequests_InvalidUseless(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodGet, "/admin/requests?useless=notabool", "admin-secret", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleListRequests_Auth(t *testing.T) {
	fs := newFakeStore()
	h := newTestServer(t, fs)

	if w := doRequest(h, http.MethodGet, "/admin/requests", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", w.Code)
	}
	if w := doRequest(h, http.MethodGet, "/admin/requests", "api-secret", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("api token: status = %d, want 401", w.Code)
	}
}

func TestHandleListRequests_StoreError(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = errors.New("boom")
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodGet, "/admin/requests", "admin-secret", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func strPtr(s string) *string { return &s }

// TestHandleListRequests_ExposesErrorToOperators is the counterpart to the
// caller-facing redaction on GET /requests/{id}: the real failure detail must
// still be reachable over HTTP, behind the admin token.
func TestHandleListRequests_ExposesErrorToOperators(t *testing.T) {
	fs := newFakeStore()
	id := uuid.New()
	fs.listResult = []store.HelpRequest{
		{ID: id, UserID: "alice", ProblemID: "p1", Status: store.StatusFailed, Error: strPtr("pgx: dial tcp 10.0.0.7:5432: refused")},
		{ID: uuid.New(), UserID: "bob", ProblemID: "p2", Status: store.StatusDone},
	}
	h := newTestServer(t, fs)

	w := doRequest(h, http.MethodGet, "/admin/requests", "admin-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 2 {
		t.Fatalf("len(resp) = %d, want 2", len(resp))
	}
	if resp[0]["error"] != "pgx: dial tcp 10.0.0.7:5432: refused" {
		t.Errorf("error = %v, want the full stored detail", resp[0]["error"])
	}
	if _, ok := resp[1]["error"]; ok {
		t.Errorf("error present on a request that has none: %v", resp[1]["error"])
	}
}
