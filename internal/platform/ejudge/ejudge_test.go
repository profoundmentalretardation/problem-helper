// Test isolation: every test spins up its own httptest.Server that replays
// bytes captured from a real, live ejudge 3.8.0 instance (see testdata/ and
// the package doc in ejudge.go for how those were captured and what the
// protocol surface turned out to be). The fixture server is a small router
// keyed on method + "action" (or, for login/submit, on POST body content)
// so each test exercises the exact same parsing path the real client uses
// against the real server — no hand-fabricated HTML.
package ejudge_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/profoundmentalretardation/problem-helper/internal/platform/ejudge"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

// clientSID/masterSID are the session ids baked into the captured login
// fixtures; the fixture server accepts only these as "logged in".
const (
	clientSID = "b8368f6d1e3e113f"
	masterSID = "d93546d50d2605a4"
)

func serveFixture(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// newFixtureServer builds the standard happy-path router used by most
// tests: real login, statement, status/submissions, submit, and run
// report/source fixtures. Individual tests override behavior by wrapping
// the returned handler or building their own mux for error-path cases.
func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/cgi-bin/new-client", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, "bad multipart body", http.StatusBadRequest)
				return
			}
		} else {
			_ = r.ParseForm()
		}

		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			if r.FormValue("password") != "ejudge" {
				serveFixture(w, fixture(t, "login_client_permission_denied.html"))
				return
			}
			serveFixture(w, fixture(t, "login_client_ok.html"))
			return
		}

		if r.Method == http.MethodPost && r.FormValue("action_40") != "" {
			code := ""
			if r.MultipartForm != nil {
				if fh := r.MultipartForm.File["file"]; len(fh) > 0 {
					f, ferr := fh[0].Open()
					if ferr == nil {
						data, _ := io.ReadAll(f)
						code = string(data)
						_ = f.Close()
					}
				}
			}
			if strings.Contains(code, "TRIGGER_DUPLICATE") {
				serveFixture(w, fixture(t, "submit_duplicate_error.html"))
				return
			}
			serveFixture(w, fixture(t, "submit_run_response.html"))
			return
		}

		switch r.URL.Query().Get("action") {
		case "139":
			switch r.URL.Query().Get("prob_id") {
			case "1":
				serveFixture(w, fixture(t, "statement_prob_a.html"))
			case "2":
				serveFixture(w, fixture(t, "statement_prob_b.html"))
			default:
				http.Error(w, "unknown prob_id", http.StatusNotFound)
			}
		default:
			http.Error(w, "unhandled new-client action", http.StatusNotFound)
		}
	})

	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			if r.FormValue("password") != "ejudge" {
				serveFixture(w, fixture(t, "login_master_permission_denied.html"))
				return
			}
			serveFixture(w, fixture(t, "login_master_ok.html"))
			return
		}

		switch r.URL.Query().Get("action") {
		case "309":
			serveFixture(w, fixture(t, "problem_stats.html"))
		case "2":
			filter := r.URL.Query().Get("filter_expr")
			switch {
			case strings.Contains(filter, `prob=="A"`):
				serveFixture(w, fixture(t, "submissions_master_filtered.html"))
			case strings.Contains(filter, `prob=="B"`):
				serveFixture(w, fixture(t, "submissions_master_filtered_unsolved.html"))
			default:
				http.Error(w, "unhandled filter_expr", http.StatusNotFound)
			}
		case "36":
			switch r.URL.Query().Get("run_id") {
			case "5":
				serveFixture(w, fixture(t, "view_source_ok.html"))
			case "6":
				serveFixture(w, fixture(t, "view_source_wa.html"))
			default:
				http.Error(w, "unhandled run_id", http.StatusNotFound)
			}
		case "37":
			switch r.URL.Query().Get("run_id") {
			case "5":
				serveFixture(w, fixture(t, "run_report_ok.html"))
			case "6":
				serveFixture(w, fixture(t, "run_report_wa.html"))
			case "7":
				serveFixture(w, fixture(t, "run_report_not_available.html"))
			case "99999":
				serveFixture(w, fixture(t, "run_report_out_of_range.html"))
			default:
				http.Error(w, "unhandled run_id", http.StatusNotFound)
			}
		default:
			http.Error(w, "unhandled new-master action", http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, srv *httptest.Server) *ejudge.Client {
	t.Helper()
	return ejudge.New(srv.URL, "ejudge", "ejudge")
}

// --- ProblemStatement --------------------------------------------------

func TestProblemStatement_ParsesTitleAndText(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.ProblemStatement(context.Background(), "1")
	if err != nil {
		t.Fatalf("ProblemStatement: %v", err)
	}
	if got.Title != "A-Sum 1" {
		t.Errorf("Title = %q, want %q", got.Title, "A-Sum 1")
	}
	if !strings.Contains(got.Text, "На стандартном потоке ввода") {
		t.Errorf("Text = %q, want it to contain the statement body", got.Text)
	}
	if strings.Contains(got.Text, "<") {
		t.Errorf("Text = %q, want HTML tags stripped", got.Text)
	}
}

func TestProblemStatement_UnknownProblem_MalformedResponse(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	_, err := c.ProblemStatement(context.Background(), "999")
	if err == nil {
		t.Fatal("expected an error for an unknown problem id")
	}
}

// --- ProblemStatus -------------------------------------------------------

func TestProblemStatus_Solved(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.ProblemStatus(context.Background(), "ejudge", "1")
	if err != nil {
		t.Fatalf("ProblemStatus: %v", err)
	}
	if !got.Solved {
		t.Errorf("Solved = false, want true (problem A has an OK run)")
	}
}

func TestProblemStatus_NotSolved(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.ProblemStatus(context.Background(), "ejudge", "2")
	if err != nil {
		t.Fatalf("ProblemStatus: %v", err)
	}
	if got.Solved {
		t.Errorf("Solved = true, want false (problem B only has Wrong answer runs)")
	}
}

// --- Submissions ---------------------------------------------------------

func TestSubmissions_IncludesCodeLanguageAndTestCounts(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.Submissions(context.Background(), "ejudge", "1", 2)
	if err != nil {
		t.Fatalf("Submissions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Submissions) = %d, want 2 (limit)", len(got))
	}

	// Newest first: run 6 (Wrong answer), then run 5 (OK).
	if got[0].ID != "6" || got[1].ID != "5" {
		t.Errorf("IDs = [%s, %s], want [6, 5]", got[0].ID, got[1].ID)
	}
	wa, ok := got[0], got[1]
	if wa.Language != "gcc" {
		t.Errorf("Language = %q, want %q", wa.Language, "gcc")
	}
	if !strings.Contains(wa.Code, "a - b") {
		t.Errorf("Code = %q, want the wrong-answer source (a - b)", wa.Code)
	}
	if wa.TestsPassed != 0 || wa.TestsTotal != 1 {
		t.Errorf("WA TestsPassed/Total = %d/%d, want 0/1 (acm scoring stops at first failure)", wa.TestsPassed, wa.TestsTotal)
	}
	if ok.TestsPassed != 5 || ok.TestsTotal != 5 {
		t.Errorf("OK TestsPassed/Total = %d/%d, want 5/5", ok.TestsPassed, ok.TestsTotal)
	}
	if ok.SubmittedAt.IsZero() {
		t.Errorf("SubmittedAt is zero, want a parsed timestamp")
	}
}

// ejudge names languages after the compiler, so the short names on a real
// C++ course are "g++"/"clang++" — characters a \w-based row regex cannot
// match. A row that fails to match is not an error, it is silently absent,
// which turned every C++ student into "no submissions" and made every
// solved C++ problem look unsolved.
func TestSubmissions_ParsesCompilerStyleLanguageNames(t *testing.T) {
	srv := newCPPFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.Submissions(context.Background(), "ejudge", "1", 2)
	if err != nil {
		t.Fatalf("Submissions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Submissions) = %d, want 2 — a g++ row must not be dropped", len(got))
	}
	for _, s := range got {
		if s.Language != "g++" {
			t.Errorf("Language = %q, want %q", s.Language, "g++")
		}
	}
}

func TestProblemStatus_SolvedWithCompilerStyleLanguageNames(t *testing.T) {
	srv := newCPPFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.ProblemStatus(context.Background(), "ejudge", "1")
	if err != nil {
		t.Fatalf("ProblemStatus: %v", err)
	}
	if !got.Solved {
		t.Error("Solved = false, want true — the OK run is a g++ submission")
	}
}

// newCPPFixtureServer is the standard fixture server with the submissions
// list swapped for one whose language column holds "g++".
func newCPPFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	base := newFixtureServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("action") == "2" &&
			strings.Contains(r.URL.Query().Get("filter_expr"), `prob=="A"`) {
			serveFixture(w, fixture(t, "submissions_master_filtered_cpp.html"))
			return
		}
		proxyTo(t, base, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { proxyTo(t, base, w, r) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// proxyTo forwards r to the base fixture server and copies the response
// back, so an override server only has to special-case the one route it
// cares about.
func proxyTo(t *testing.T, base *httptest.Server, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body io.Reader
	if r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body", http.StatusInternalServerError)
			return
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base.URL+r.URL.RequestURI(), body)
	if err != nil {
		http.Error(w, "building proxy request", http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := base.Client().Do(req)
	if err != nil {
		http.Error(w, "proxying", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- SubmitAsSystem -------------------------------------------------------

func TestSubmitAsSystem_ReturnsNewRunID(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.SubmitAsSystem(context.Background(), "1", "int main(){return 0;}", "gcc")
	if err != nil {
		t.Fatalf("SubmitAsSystem: %v", err)
	}
	if got.ID != "5" {
		t.Errorf("ID = %q, want %q (newest row in the post-submit table)", got.ID, "5")
	}
	if got.Done {
		t.Errorf("Done = true, want false (just submitted, not yet judged)")
	}
}

func TestSubmitAsSystem_UnknownLanguage(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	_, err := c.SubmitAsSystem(context.Background(), "1", "code", "not-a-real-language")
	if !errors.Is(err, ejudge.ErrUnknownLanguage) {
		t.Errorf("err = %v, want ErrUnknownLanguage", err)
	}
}

func TestSubmitAsSystem_Duplicate(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	_, err := c.SubmitAsSystem(context.Background(), "1", "TRIGGER_DUPLICATE", "gcc")
	if !errors.Is(err, ejudge.ErrDuplicateSubmission) {
		t.Errorf("err = %v, want ErrDuplicateSubmission", err)
	}
}

// --- RunResult -------------------------------------------------------------

func TestRunResult_OK(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.RunResult(context.Background(), "5")
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}
	if !got.Done || !got.Passed {
		t.Errorf("Done/Passed = %v/%v, want true/true", got.Done, got.Passed)
	}
	if got.TestsPassed != 5 || got.TestsTotal != 5 {
		t.Errorf("TestsPassed/Total = %d/%d, want 5/5", got.TestsPassed, got.TestsTotal)
	}
}

func TestRunResult_WrongAnswer(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.RunResult(context.Background(), "6")
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}
	if !got.Done || got.Passed {
		t.Errorf("Done/Passed = %v/%v, want true/false", got.Done, got.Passed)
	}
	if got.TestsPassed != 0 || got.TestsTotal != 1 {
		t.Errorf("TestsPassed/Total = %d/%d, want 0/1", got.TestsPassed, got.TestsTotal)
	}
}

// TestRunResult_StuckInQueue is the "run stuck in queue" error-case test:
// a run still compiling/running has no report yet ("Report is not
// available" — captured live by polling immediately after submit). This
// must NOT surface as an error; the worker polls again later.
func TestRunResult_StuckInQueue(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.RunResult(context.Background(), "7")
	if err != nil {
		t.Fatalf("RunResult: unexpected error for an in-progress run: %v", err)
	}
	if got.Done {
		t.Errorf("Done = true, want false for a run still being judged")
	}
}

func TestRunResult_NotFound(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	_, err := c.RunResult(context.Background(), "99999")
	if !errors.Is(err, ejudge.ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

// --- TestResult -------------------------------------------------------------

func TestTestResult_OK_FullDetail(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.TestResult(context.Background(), "5", 1)
	if err != nil {
		t.Fatalf("TestResult: %v", err)
	}
	if got.Verdict != "OK" {
		t.Errorf("Verdict = %q, want %q", got.Verdict, "OK")
	}
	// Byte-exact against the real test file on disk (001.dat is "1\n1\n\n",
	// 5 bytes) — verified against the live container, not assumed.
	if got.Input != "1\n1\n\n" {
		t.Errorf("Input = %q, want %q", got.Input, "1\n1\n\n")
	}
	if got.Expected != "2\n" {
		t.Errorf("Expected = %q, want %q", got.Expected, "2\n")
	}
	if got.Actual != got.Expected {
		t.Errorf("Actual = %q, want it to match Expected on a passing test", got.Actual)
	}
}

func TestTestResult_WrongAnswer_ActualDiffersFromExpected(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	got, err := c.TestResult(context.Background(), "6", 1)
	if err != nil {
		t.Fatalf("TestResult: %v", err)
	}
	if got.Verdict == "OK" {
		t.Errorf("Verdict = OK, want a failing verdict")
	}
	if got.Actual == got.Expected {
		t.Errorf("Actual = Expected = %q, want them to differ on a wrong-answer test", got.Actual)
	}
}

func TestTestResult_UnjudgedRun(t *testing.T) {
	srv := newFixtureServer(t)
	c := newClient(t, srv)

	_, err := c.TestResult(context.Background(), "7", 1)
	if err == nil {
		t.Fatal("expected an error asking for test detail on a run with no report yet")
	}
}

// --- Auth failures -----------------------------------------------------

func TestSubmitAsSystem_ClientAuthFailure(t *testing.T) {
	srv := newFixtureServer(t)
	c := ejudge.New(srv.URL, "ejudge", "WRONGPASS")

	_, err := c.SubmitAsSystem(context.Background(), "1", "code", "gcc")
	if !errors.Is(err, ejudge.ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestProblemStatus_MasterAuthFailure(t *testing.T) {
	srv := newFixtureServer(t)
	c := ejudge.New(srv.URL, "ejudge", "WRONGPASS")

	_, err := c.ProblemStatus(context.Background(), "ejudge", "1")
	if !errors.Is(err, ejudge.ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

// --- Session expiry: transparent re-login and retry ------------------------

// TestRunResult_SessionExpired_RelogsInAndRetries simulates a master
// session that ejudge has invalidated server-side (captured live via a
// deliberately bad SID — "Error: Invalid session"): the first request
// fails, the client must re-login and retry once, transparently.
func TestRunResult_SessionExpired_RelogsInAndRetries(t *testing.T) {
	var reportRequests int64

	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_master_ok.html"))
			return
		}
		if r.URL.Query().Get("action") == "37" && r.URL.Query().Get("run_id") == "5" {
			if atomic.AddInt64(&reportRequests, 1) == 1 {
				serveFixture(w, fixture(t, "session_invalid_master.html"))
				return
			}
			serveFixture(w, fixture(t, "run_report_ok.html"))
			return
		}
		http.Error(w, "unhandled", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge")
	got, err := c.RunResult(context.Background(), "5")
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}
	if !got.Done || !got.Passed {
		t.Errorf("Done/Passed = %v/%v, want true/true after transparent relogin", got.Done, got.Passed)
	}
	if reportRequests != 2 {
		t.Errorf("report requests = %d, want 2 (initial + retry after relogin)", reportRequests)
	}
}

// --- Malformed responses -----------------------------------------------

func TestProblemStatement_MalformedResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-client", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_client_ok.html"))
			return
		}
		serveFixture(w, "<html><body>not the page we expected</body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge")
	_, err := c.ProblemStatement(context.Background(), "1")
	if !errors.Is(err, ejudge.ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestRunResult_MalformedResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_master_ok.html"))
			return
		}
		serveFixture(w, "<html><body>completely unexpected shape</body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge")
	_, err := c.RunResult(context.Background(), "5")
	if !errors.Is(err, ejudge.ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
}

// ejudge renders its error pages in exactly the <h2><font color="red">
// shape a real verdict uses. Reading one back as a judged failing run makes
// a transient platform outage look like "the student's fix didn't work" —
// the repair loop burns a retry and the request lands in no_fix instead of
// failed, which is precisely the distinction the pipeline exists to keep.
func TestRunResult_ErrorPageIsNotAVerdict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_master_ok.html"))
			return
		}
		serveFixture(w, fixture(t, "run_report_server_error.html"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge")
	got, err := c.RunResult(context.Background(), "5")
	if !errors.Is(err, ejudge.ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse (got result %+v)", err, got)
	}
}

// The declared "size N" counts raw bytes, but the page carries the content
// HTML-escaped, where '<' takes 4 bytes and '&' takes 5. Slicing the
// escaped text by the raw size truncates — usually mid-entity — and hands
// the repair model corrupted test data for exactly the test it is
// diagnosing.
func TestTestResult_TestDataContainingHTMLSpecialCharacters(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-master", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_master_ok.html"))
			return
		}
		serveFixture(w, fixture(t, "run_report_wa_escaped.html"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge")
	got, err := c.TestResult(context.Background(), "6", 1)
	if err != nil {
		t.Fatalf("TestResult: %v", err)
	}
	if want := "a<b&c\n"; got.Input != want {
		t.Errorf("Input = %q, want %q", got.Input, want)
	}
}

// --- Timeout -------------------------------------------------------------

func TestProblemStatement_Timeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/new-client", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == http.MethodPost && r.FormValue("login") != "" {
			serveFixture(w, fixture(t, "login_client_ok.html"))
			return
		}
		time.Sleep(200 * time.Millisecond)
		serveFixture(w, fixture(t, "statement_prob_a.html"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ejudge.New(srv.URL, "ejudge", "ejudge", ejudge.WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ProblemStatement(ctx, "1")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !os.IsTimeout(err) && !isTimeoutErr(err) {
		t.Errorf("err = %v, want a timeout-flavored error", err)
	}
}

func isTimeoutErr(err error) bool {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout exceeded")
}

// --- Sanity: the fixture router itself matches real ejudge query shapes ---

func TestFixtureServer_ProblemStatsRoundTrip(t *testing.T) {
	srv := newFixtureServer(t)
	resp, err := http.Get(srv.URL + "/cgi-bin/new-master?" + url.Values{
		"SID":    {masterSID},
		"action": {"309"},
	}.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestSubmissions_RejectsLoginsThatCouldBreakOutOfTheFilter is the must-catch
// half of the filter_expr injection guard: userID reaches us from the
// untrusted POST /help body and is interpolated into an expression ejudge
// parses server-side, where Go's %q escaping does not apply. A crafted login
// must be refused before any request goes out, not merely quoted.
func TestSubmissions_RejectsLoginsThatCouldBreakOutOfTheFilter(t *testing.T) {
	cases := []struct {
		name  string
		login string
	}{
		{"quote and boolean widening", `ejudge"||1==1||"`},
		{"quote then always-true tail", `ejudge" || login=="victim`},
		{"embedded backslash", `ejudge\"`},
		{"whitespace and operators", `ejudge || 1`},
		{"empty", ``},
		{"over the length limit", strings.Repeat("a", 65)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			}))
			t.Cleanup(srv.Close)

			_, err := ejudge.New(srv.URL, "ejudge", "ejudge").
				Submissions(context.Background(), tc.login, "1", 2)
			if err == nil {
				t.Fatalf("Submissions(%q) succeeded, want rejection", tc.login)
			}
			if reached {
				t.Errorf("Submissions(%q) issued an HTTP request; the login must be refused first", tc.login)
			}
		})
	}
}

// TestSubmissions_AcceptsLegitimateLogins is the must-pass half: the guard
// must not reject the login shapes ejudge actually issues.
func TestSubmissions_AcceptsLegitimateLogins(t *testing.T) {
	for _, login := range []string{"ejudge", "first.last@example.org", "user-1", "user_1", strings.Repeat("a", 64)} {
		t.Run(login, func(t *testing.T) {
			srv := newFixtureServer(t)
			c := newClient(t, srv)

			// The fixture server only knows the "ejudge" login's runs, so a
			// non-matching login legitimately yields no rows — what matters
			// is that the call is not rejected out of hand.
			if _, err := c.Submissions(context.Background(), login, "1", 2); err != nil {
				t.Errorf("Submissions(%q): %v, want the login accepted", login, err)
			}
		})
	}
}
