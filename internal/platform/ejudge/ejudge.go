// Package ejudge implements platform.Platform against a real ejudge
// instance (ejudge.ru), the first and only Task-15-verified judge backend.
//
// Protocol surface: ejudge 3.8.0 exposes two classic CGI programs driven by
// HTML forms and numeric "action" query parameters — there is no usable
// JSON API for this flow (the *_JSON actions exist in the protocol header
// but proved unreliable/undocumented against a live 3.8.0 instance; see
// docs/plans/20260729-mvp-service.md Task 15 notes). This client scrapes
// HTML instead, mirroring what a browser driving the CGI UI would do:
//
//   - cgi-bin/new-client (the "participant" role, role=0): problem
//     statements and submitting code. The system user logs in here to
//     fetch a problem's statement and to submit repair-loop verification
//     runs, exactly as a student would submit their own solution.
//   - cgi-bin/new-master (the "Administrator" role, role=6): everything
//     that needs to see another user's data — a student's submissions,
//     whether they solved a problem, a run's verdict, and full per-test
//     input/expected/actual detail. The same EJUDGE_SYSTEM_LOGIN account
//     logs into both roles under separate sessions.
//
// This sandbox's ejudge instance only has one registered user (the "ejudge"
// administrator, also a contest participant), so every captured fixture
// necessarily uses that single login for both "system user" and "student"
// roles. The query surface itself (new-master's filter_expr, scoped by
// login/prob) is user-parameterized and works identically for any login;
// nothing here is hardwired to that one account beyond the fixtures.
//
// Known degradation: this contest is configured with acm-style scoring,
// which halts judging at the first failing test. For a submission that
// doesn't pass every test, RunResult/Submission's TestsTotal is therefore
// "tests actually judged before ejudge stopped", not the problem's true
// hidden test count — ejudge exposes no endpoint (even to Administrator)
// that reports the true total independently of a run's outcome. This is
// the same class of degradation the plan anticipates for TestResult on
// judges that hide test data; it does not affect pick.Best, which only
// compares TestsPassed across a single problem's submissions.
package ejudge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/profoundmentalretardation/problem-helper/internal/platform"
)

// Sentinel errors. Every method wraps the underlying failure with one of
// these so callers (the worker) can tell "our infra/credentials are broken"
// (failed) apart from platform-reported outcomes.
var (
	// ErrAuthFailed is returned when ejudge rejects EJUDGE_SYSTEM_LOGIN /
	// EJUDGE_SYSTEM_PASSWORD ("Permission denied" at login time).
	ErrAuthFailed = errors.New("ejudge: authentication failed")

	// ErrMalformedResponse is returned when a response doesn't match any
	// known shape (success or documented error) — an ejudge upgrade or an
	// unexpected page changed the HTML this client depends on.
	ErrMalformedResponse = errors.New("ejudge: malformed response")

	// ErrRunNotFound is returned when ejudge reports a run id as out of
	// range for the contest (never submitted, or a different contest). It
	// wraps platform.ErrRunNotFound so backend-agnostic callers (the repair
	// loop's crash resume) can recognise it without importing this package.
	ErrRunNotFound = fmt.Errorf("ejudge: %w", platform.ErrRunNotFound)

	// ErrDuplicateSubmission is returned when ejudge refuses a submission
	// because its content is byte-identical to an existing run
	// (ignore_duplicated_runs in this course's serve.cfg). It wraps
	// platform.ErrDuplicateSubmission so backend-agnostic callers (the
	// repair loop) can recognise it without importing this package.
	ErrDuplicateSubmission = fmt.Errorf("ejudge: %w", platform.ErrDuplicateSubmission)

	// ErrUnknownLanguage is returned when SubmitAsSystem is asked to
	// submit in a language short name the contest's problem page doesn't
	// offer.
	ErrUnknownLanguage = errors.New("ejudge: unknown language")
)

const (
	defaultContestID = "1"
	defaultTimeout   = 30 * time.Second

	// Action codes from ejudge's new_server_proto.h (NEW_SRV_ACTION_*).
	// Named here instead of magic numbers so the protocol mapping is
	// auditable against that header. Submitting code (NEW_SRV_ACTION_
	// SUBMIT_RUN = 40) has no query-string action parameter of its own —
	// the browser form instead POSTs a button field named "action_40",
	// used literally in submitMultipart.
	actionViewProblemSubmit = 139 // statement + submit form (participant)
	actionViewSource        = 36  // full source + brief run info (master)
	actionViewReport        = 37  // verdict + per-test detail (master)
	actionMainPage          = 2   // filterable submissions list (master)
	actionProblemStats      = 309 // numeric prob_id -> short_name (master)
)

// Client is the real ejudge.Platform implementation. Zero value is not
// usable; construct with New.
type Client struct {
	baseURL    string
	login      string
	password   string
	contestID  string
	httpClient *http.Client

	mu           sync.Mutex
	clientSID    string            // participant session (new-client)
	masterSID    string            // Administrator session (new-master)
	problemNames map[string]string // numeric prob_id -> short_name, cached
}

// Option configures a Client beyond the required credentials.
type Option func(*Client)

// WithContestID overrides the default contest ("1"). ejudge scopes an
// entire session to one contest; this MVP targets a single course/contest,
// so New defaults to "1" and this option exists for tests and any future
// multi-contest wiring.
func WithContestID(id string) Option {
	return func(c *Client) { c.contestID = id }
}

// WithHTTPClient overrides the default HTTP client (30s timeout). Tests use
// this to inject a short-timeout client for the timeout error-case test.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New builds a Client against baseURL (e.g. "http://ejudge-host"), logging
// in as login/password lazily on first use.
func New(baseURL, login, password string, opts ...Option) *Client {
	c := &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		login:        login,
		password:     password,
		contestID:    defaultContestID,
		httpClient:   &http.Client{Timeout: defaultTimeout},
		problemNames: map[string]string{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var _ platform.Platform = (*Client)(nil)

// ---------------------------------------------------------------------
// Platform interface
// ---------------------------------------------------------------------

// ProblemStatement fetches problemID's statement via the participant
// session's "submit a solution" page (action=139), which carries the
// rendered statement alongside the submit form.
func (c *Client) ProblemStatement(ctx context.Context, problemID string) (platform.Statement, error) {
	page, err := c.fetchSubmitPage(ctx, problemID)
	if err != nil {
		return platform.Statement{}, err
	}
	return platform.Statement{
		ProblemID: problemID,
		Title:     page.title,
		Text:      page.text,
	}, nil
}

// ProblemStatus reports whether userID has an "OK" run for problemID,
// scoped via new-master's filter_expr (login=="userID" && prob=="short").
func (c *Client) ProblemStatus(ctx context.Context, userID, problemID string) (platform.Status, error) {
	rows, err := c.listMasterSubmissions(ctx, userID, problemID)
	if err != nil {
		return platform.Status{}, err
	}
	for _, r := range rows {
		if r.result == "OK" {
			return platform.Status{Solved: true}, nil
		}
	}
	return platform.Status{Solved: false}, nil
}

// Submissions returns up to limit of userID's submissions to problemID,
// newest first, each enriched with source code (action=36) and a
// tests-passed/tests-total count derived from the run report (action=37).
func (c *Client) Submissions(ctx context.Context, userID, problemID string, limit int) ([]platform.Submission, error) {
	rows, err := c.listMasterSubmissions(ctx, userID, problemID)
	if err != nil {
		return nil, err
	}
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}

	subs := make([]platform.Submission, 0, len(rows))
	for _, r := range rows {
		src, err := c.fetchViewSource(ctx, r.runID)
		if err != nil {
			return nil, err
		}
		passed, total, err := c.fetchTestCounts(ctx, r.runID)
		if err != nil {
			return nil, err
		}
		subs = append(subs, platform.Submission{
			ID:          r.runID,
			Code:        src.code,
			Language:    r.language,
			TestsPassed: passed,
			TestsTotal:  total,
			SubmittedAt: src.submittedAt,
		})
	}
	return subs, nil
}

// SubmitAsSystem submits code in lang (a language short_name, e.g. "gcc")
// for problemID under the system user's participant session, the same
// path a student's own submission takes so verification runs are judged
// identically.
func (c *Client) SubmitAsSystem(ctx context.Context, problemID, code, lang string) (platform.RunResult, error) {
	page, err := c.fetchSubmitPage(ctx, problemID)
	if err != nil {
		return platform.RunResult{}, err
	}
	langID, ok := page.languageIDs[lang]
	if !ok {
		return platform.RunResult{}, fmt.Errorf("%w: %q", ErrUnknownLanguage, lang)
	}

	body, err := c.submitWithSession(ctx, problemID, langID, code)
	if err != nil {
		return platform.RunResult{}, err
	}

	if strings.Contains(body, "duplicate of another run") {
		return platform.RunResult{}, ErrDuplicateSubmission
	}
	if strings.Contains(body, "Permission denied") {
		return platform.RunResult{}, ErrAuthFailed
	}

	runID, err := parseNewestRunID(body)
	if err != nil {
		return platform.RunResult{}, err
	}
	// The run id comes from the newest row of a table the whole service
	// shares, since every verification submits under one system login. If
	// that row is not newer than the one on the pre-submit page it belongs
	// to some other submission (a concurrent repair of the same problem, or
	// a submit ejudge silently dropped), and polling it would "verify" a run
	// that never contained this code.
	if !isNewerRunID(runID, page.newestRunID) {
		return platform.RunResult{}, fmt.Errorf(
			"ejudge: submit returned run %s, not newer than pre-submit run %s: %w",
			runID, page.newestRunID, ErrMalformedResponse)
	}
	return platform.RunResult{ID: runID, Done: false}, nil
}

// submitWithSession POSTs the solution, transparently re-logging in and
// re-posting once if the participant session expired in the meantime. A
// submit happens minutes into a run — long after ensureClientSession's own
// login — so it is the call most exposed to expiry, and without this an
// expired session surfaces as ErrAuthFailed and puts an otherwise healthy
// request in status=failed.
func (c *Client) submitWithSession(ctx context.Context, problemID, langID, code string) (string, error) {
	sid, err := c.ensureClientSession(ctx)
	if err != nil {
		return "", err
	}
	body, err := c.submitMultipart(ctx, sid, problemID, langID, code)
	if err != nil {
		return "", err
	}
	if !hasErrorTitle(body, "Error: Invalid session") {
		return body, nil
	}

	c.mu.Lock()
	c.clientSID = ""
	c.mu.Unlock()
	sid, err = c.loginClient(ctx)
	if err != nil {
		return "", err
	}
	return c.submitMultipart(ctx, sid, problemID, langID, code)
}

// isNewerRunID reports whether runID is a later run than floor. An empty
// floor (no previous submissions) accepts anything; ids ejudge did not
// render as plain integers fall back to "different is good enough" rather
// than rejecting a submit that probably succeeded.
func isNewerRunID(runID, floor string) bool {
	if floor == "" {
		return true
	}
	got, err1 := strconv.Atoi(runID)
	prev, err2 := strconv.Atoi(floor)
	if err1 != nil || err2 != nil {
		return runID != floor
	}
	return got > prev
}

// RunResult polls a run's state via new-master's report page (action=37).
// A run still queued/compiling/running has no report yet — ejudge answers
// "Report is not available", which this maps to Done=false rather than an
// error so a caller can poll indefinitely (or give up on its own budget)
// without ejudge.go ever surfacing "stuck in queue" as a failure.
func (c *Client) RunResult(ctx context.Context, runID string) (platform.RunResult, error) {
	body, err := c.masterGet(ctx, actionViewReport, url.Values{"run_id": {runID}})
	if err != nil {
		return platform.RunResult{}, err
	}

	if hasErrorTitle(body, "Report is not available") {
		return platform.RunResult{ID: runID, Done: false}, nil
	}
	if hasErrorDetail(body, "is out of range") {
		return platform.RunResult{}, fmt.Errorf("%w: run %s: %w", ErrRunNotFound, runID, ErrMalformedResponse)
	}

	verdict, err := parseReportVerdict(body)
	if err != nil {
		return platform.RunResult{}, err
	}
	passed, total := countReportTests(body)
	return platform.RunResult{
		ID:          runID,
		Done:        true,
		Passed:      verdict == "OK",
		TestsPassed: passed,
		TestsTotal:  total,
	}, nil
}

// TestResult returns per-test detail for a run's already-judged test. Full
// input/expected/actual is available to the Administrator role on this
// contest; if a future course configuration hides it even from
// Administrator, the verdict/index are still returned and the text fields
// are left empty rather than erroring — the degradation the plan
// anticipates for judges that hide test data.
func (c *Client) TestResult(ctx context.Context, runID string, testID int) (platform.TestCase, error) {
	body, err := c.masterGet(ctx, actionViewReport, url.Values{"run_id": {runID}})
	if err != nil {
		return platform.TestCase{}, err
	}
	if hasErrorTitle(body, "Report is not available") {
		return platform.TestCase{}, fmt.Errorf("ejudge: run %s test %d: %w", runID, testID, ErrMalformedResponse)
	}

	verdict, ok := reportTestVerdict(body, testID)
	if !ok {
		return platform.TestCase{}, fmt.Errorf("ejudge: run %s has no test %d", runID, testID)
	}

	tc := platform.TestCase{Index: testID, Verdict: verdict}
	if in, out, correct, ok := reportTestDetail(body, testID); ok {
		tc.Input = in
		tc.Actual = out
		tc.Expected = correct
	}
	return tc, nil
}

// ---------------------------------------------------------------------
// Sessions and low-level HTTP
// ---------------------------------------------------------------------

var sidPattern = regexp.MustCompile(`SID=['"]([0-9a-fA-F]+)['"]`)

func (c *Client) ensureClientSession(ctx context.Context) (string, error) {
	c.mu.Lock()
	sid := c.clientSID
	c.mu.Unlock()
	if sid != "" {
		return sid, nil
	}
	return c.loginClient(ctx)
}

func (c *Client) ensureMasterSession(ctx context.Context) (string, error) {
	c.mu.Lock()
	sid := c.masterSID
	c.mu.Unlock()
	if sid != "" {
		return sid, nil
	}
	return c.loginMaster(ctx)
}

func (c *Client) loginClient(ctx context.Context) (string, error) {
	form := url.Values{
		"contest_id": {c.contestID},
		"role":       {"0"},
		"prob_name":  {""},
		"login":      {c.login},
		"password":   {c.password},
		"locale_id":  {"0"},
		"action_2":   {"Log in"},
	}
	body, err := c.postForm(ctx, c.baseURL+"/cgi-bin/new-client", form)
	if err != nil {
		return "", err
	}
	sid, err := extractSID(body)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.clientSID = sid
	c.mu.Unlock()
	return sid, nil
}

func (c *Client) loginMaster(ctx context.Context) (string, error) {
	form := url.Values{
		"login":      {c.login},
		"password":   {c.password},
		"contest_id": {c.contestID},
		"role":       {"6"}, // Administrator
		"locale_id":  {"0"},
		"action_2":   {"Submit"},
	}
	body, err := c.postForm(ctx, c.baseURL+"/cgi-bin/new-master", form)
	if err != nil {
		return "", err
	}
	sid, err := extractSID(body)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.masterSID = sid
	c.mu.Unlock()
	return sid, nil
}

func extractSID(body string) (string, error) {
	if strings.Contains(body, "Permission denied") {
		return "", ErrAuthFailed
	}
	m := sidPattern.FindStringSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("ejudge: login response: %w", ErrMalformedResponse)
	}
	return m[1], nil
}

// masterGet performs a GET against new-master?action=<action>&SID=<sid>&...,
// logging in lazily and transparently re-logging in once if the session
// has expired.
func (c *Client) masterGet(ctx context.Context, action int, params url.Values) (string, error) {
	sid, err := c.ensureMasterSession(ctx)
	if err != nil {
		return "", err
	}
	body, err := c.get(ctx, c.baseURL+"/cgi-bin/new-master", sid, action, params)
	if err != nil {
		return "", err
	}
	if hasErrorTitle(body, "Error: Invalid session") {
		c.mu.Lock()
		c.masterSID = ""
		c.mu.Unlock()
		sid, err = c.loginMaster(ctx)
		if err != nil {
			return "", err
		}
		body, err = c.get(ctx, c.baseURL+"/cgi-bin/new-master", sid, action, params)
		if err != nil {
			return "", err
		}
	}
	return body, nil
}

func (c *Client) clientGet(ctx context.Context, action int, params url.Values) (string, error) {
	sid, err := c.ensureClientSession(ctx)
	if err != nil {
		return "", err
	}
	body, err := c.get(ctx, c.baseURL+"/cgi-bin/new-client", sid, action, params)
	if err != nil {
		return "", err
	}
	if hasErrorTitle(body, "Error: Invalid session") {
		c.mu.Lock()
		c.clientSID = ""
		c.mu.Unlock()
		sid, err = c.loginClient(ctx)
		if err != nil {
			return "", err
		}
		body, err = c.get(ctx, c.baseURL+"/cgi-bin/new-client", sid, action, params)
		if err != nil {
			return "", err
		}
	}
	return body, nil
}

func (c *Client) get(ctx context.Context, endpoint, sid string, action int, params url.Values) (string, error) {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("SID", sid)
	q.Set("action", strconv.Itoa(action))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("ejudge: building request: %w", err)
	}
	return c.doWithRetry(req)
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("ejudge: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.doWithRetry(req)
}

// submitMultipart POSTs a solution the same way the browser's "Submit a
// solution" form does: SID, prob_id, lang_id, the code as an uploaded
// file, and the action_40 button field.
func (c *Client) submitMultipart(ctx context.Context, sid, probID, langID, code string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]string{
		"SID":       sid,
		"prob_id":   probID,
		"lang_id":   langID,
		"action_40": "Send!",
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return "", fmt.Errorf("ejudge: encoding submission: %w", err)
		}
	}
	fw, err := w.CreateFormFile("file", "solution.txt")
	if err != nil {
		return "", fmt.Errorf("ejudge: encoding submission: %w", err)
	}
	if _, err := fw.Write([]byte(code)); err != nil {
		return "", fmt.Errorf("ejudge: encoding submission: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("ejudge: encoding submission: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/cgi-bin/new-client", &buf)
	if err != nil {
		return "", fmt.Errorf("ejudge: building request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.doWithRetry(req)
}

// doWithRetry retries transient (network-level) failures a couple of
// times with a short backoff; it never retries once the server has
// responded, successfully or not — those are ejudge's real answer.
//
// A POST body is a one-shot reader: after the first attempt it is drained
// and closed, so every retry rebuilds the body from req.GetBody (which
// http.NewRequest populates for the in-memory body types this package
// uses). A request whose body cannot be replayed is not retried — resending
// it would silently submit an empty form.
func (c *Client) doWithRetry(req *http.Request) (string, error) {
	const maxAttempts = 3
	backoff := 100 * time.Millisecond

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-req.Context().Done():
				return "", fmt.Errorf("ejudge: %w", req.Context().Err())
			case <-time.After(backoff):
			}
			backoff *= 2

			if req.Body != nil {
				if req.GetBody == nil {
					return "", fmt.Errorf("ejudge: request to %s failed and its body cannot be replayed: %w", req.URL.Path, lastErr)
				}
				body, err := req.GetBody()
				if err != nil {
					return "", fmt.Errorf("ejudge: rebuilding request body for %s: %w", req.URL.Path, err)
				}
				req.Body = body
			}
		}

		body, err := c.doOnce(req)
		if err != nil {
			lastErr = err
			if errors.Is(req.Context().Err(), context.DeadlineExceeded) || errors.Is(req.Context().Err(), context.Canceled) {
				return "", fmt.Errorf("ejudge: request to %s: %w", req.URL.Path, err)
			}
			// The server already received and acted on this request; the
			// failure was in reading or in its status code, not in delivery.
			// Retrying would re-POST a submission ejudge has already queued —
			// either creating a second run under the shared system login (so
			// the run-id floor now has two candidates) or drawing
			// ErrDuplicateSubmission for an attempt that actually succeeded.
			var responded *respondedError
			if errors.As(err, &responded) {
				return "", fmt.Errorf("ejudge: request to %s: %w", req.URL.Path, err)
			}
			continue // transient network error, retry
		}
		return body, nil
	}
	return "", fmt.Errorf("ejudge: request to %s failed after %d attempts: %w", req.URL.Path, maxAttempts, lastErr)
}

// doOnce performs a single attempt, closing the response body before it
// returns — a defer inside doWithRetry's loop would hold every attempt's
// connection open until the whole retry sequence finished.
func (c *Client) doOnce(req *http.Request) (string, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &respondedError{err}
	}
	// ejudge signals all of its own error conditions with a 200 and an error
	// page, so a 4xx/5xx here is something in front of it — an nginx 502, a
	// CGI 500. isErrorPage does not recognise those, so without this check a
	// proxy error page flows on into the parsers: countReportTests finds no
	// rows and reports (0, 0) with no error, every submission looks like a
	// compile error, and pick.Best chooses on garbage.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &respondedError{fmt.Errorf("%w: %s returned %s", ErrMalformedResponse, req.URL.Path, resp.Status)}
	}
	return string(data), nil
}

// respondedError marks a failure that happened after the server had already
// received the request, so doWithRetry knows the request must not be
// replayed. See the retry loop above for why that matters for submits.
type respondedError struct{ err error }

func (e *respondedError) Error() string { return e.err.Error() }
func (e *respondedError) Unwrap() error { return e.err }

// ---------------------------------------------------------------------
// Page parsing
// ---------------------------------------------------------------------

type submitPage struct {
	title       string
	text        string
	languageIDs map[string]string // short_name -> lang_id
	// newestRunID is the top row of this page's "Previous submissions"
	// table, i.e. the newest run the system user had for this problem
	// before we posted anything. Empty when the table is absent (nothing
	// submitted yet). SubmitAsSystem uses it as a floor to tell its own run
	// apart from someone else's.
	newestRunID string
}

var (
	titleRe     = regexp.MustCompile(`<h2>Submit a solution for (.+?)</h2>`)
	statementRe = regexp.MustCompile(`(?s)</table>\s*(.*?)\s*<h3>Submit a solution</h3>`)
	// Attribute order varies: the browser's last-used language renders as
	// <option selected="selected" value="N">, everything else as plain
	// <option value="N"> — match value="N" anywhere in the tag.
	langOptionRe = regexp.MustCompile(`<option[^>]*\bvalue="(\d+)"[^>]*>([^<]*)</option>`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

func (c *Client) fetchSubmitPage(ctx context.Context, problemID string) (submitPage, error) {
	body, err := c.clientGet(ctx, actionViewProblemSubmit, url.Values{"prob_id": {problemID}})
	if err != nil {
		return submitPage{}, err
	}
	if strings.Contains(body, "Invalid contest") || strings.Contains(body, "Invalid problem") {
		return submitPage{}, fmt.Errorf("ejudge: problem %q: %w", problemID, ErrMalformedResponse)
	}

	tm := titleRe.FindStringSubmatch(body)
	if tm == nil {
		return submitPage{}, fmt.Errorf("ejudge: problem %q statement: %w", problemID, ErrMalformedResponse)
	}

	text := ""
	if sm := statementRe.FindStringSubmatch(body); sm != nil {
		text = htmlToText(sm[1])
	}

	langIDs := map[string]string{}
	for _, m := range langOptionRe.FindAllStringSubmatch(body, -1) {
		id, label := m[1], m[2]
		if id == "" || label == "" {
			continue
		}
		short := strings.TrimSpace(strings.SplitN(label, " - ", 2)[0])
		if short != "" {
			langIDs[short] = id
		}
	}

	// A first-ever submission has no "Previous submissions" table; that is
	// not an error here, just an empty floor.
	newest, _ := parseNewestRunID(body)

	return submitPage{title: tm[1], text: text, languageIDs: langIDs, newestRunID: newest}, nil
}

// htmlToText strips tags and decodes entities for prompt-context display;
// it is not meant to losslessly round-trip formatting.
func htmlToText(fragment string) string {
	noTags := tagRe.ReplaceAllString(fragment, "\n")
	decoded := html.UnescapeString(noTags)
	decoded = strings.ReplaceAll(decoded, " ", " ")
	decoded = blankLinesRe.ReplaceAllString(decoded, "\n\n")
	return strings.TrimSpace(decoded)
}

// parseNewestRunID reads the first (newest) row of the per-problem
// "Previous submissions" table rendered after a successful submit.
var submitRunRowRe = regexp.MustCompile(`<td class="b1">(\d+)</td>`)

func parseNewestRunID(body string) (string, error) {
	i := strings.Index(body, "Previous submissions of this problem")
	if i < 0 {
		return "", fmt.Errorf("ejudge: submit response: %w", ErrMalformedResponse)
	}
	m := submitRunRowRe.FindStringSubmatch(body[i:])
	if m == nil {
		return "", fmt.Errorf("ejudge: submit response: %w", ErrMalformedResponse)
	}
	return m[1], nil
}

// masterSubmissionRow is one row of new-master's filterable submissions
// table (action=2, filter_expr).
type masterSubmissionRow struct {
	runID    string
	language string
	result   string
	failed   string
}

var masterRowRe = regexp.MustCompile(`(?s)<tr><td class="b1">(\d+)</td>` +
	`<td class="b1">[^<]*</td>` + // Time (elapsed, not absolute — not used)
	`<td class="b1">[^<]*</td>` + // User name
	`<td class="b1">([^<]+)</td>` + // Problem short name
	// Language short name. Must not be `\w+`: ejudge names languages after
	// the compiler, so the real values include `g++`, `clang++`, `fbc-32`
	// and `make-vg` — a `\w+` here fails the whole row alternation and
	// silently drops every C++ submission from the list.
	`<td class="b1">([^<]+)</td>` +
	`<td class="b1"><a[^>]*>([^<]+)</a>.*?</td>` + // Result text
	`<td class="b1">([^<]+)</td>`) // Failed test

func (c *Client) resolveProblemShortName(ctx context.Context, problemID string) (string, error) {
	c.mu.Lock()
	name, ok := c.problemNames[problemID]
	c.mu.Unlock()
	if ok {
		return name, nil
	}

	body, err := c.masterGet(ctx, actionProblemStats, nil)
	if err != nil {
		return "", err
	}
	names := parseProblemStats(body)
	c.mu.Lock()
	for id, short := range names {
		c.problemNames[id] = short
	}
	name, ok = c.problemNames[problemID]
	c.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("ejudge: unknown problem id %q", problemID)
	}
	return name, nil
}

var problemStatsRowRe = regexp.MustCompile(`(?s)<td class="b1">(\d+)</td>\s*<td class="b1">([^<]+)</td>\s*<td class="b1">[^<]*</td>`)

func parseProblemStats(body string) map[string]string {
	out := map[string]string{}
	for _, m := range problemStatsRowRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// loginRe is the character set an ejudge login may use. userID reaches us
// from the untrusted POST /help body and is interpolated into a filter
// expression that ejudge parses and evaluates server-side; Go's %q escaping
// is not ejudge's, so a crafted login could otherwise break out of the
// string literal and widen the filter to another student's runs.
var loginRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func (c *Client) listMasterSubmissions(ctx context.Context, userID, problemID string) ([]masterSubmissionRow, error) {
	if !loginRe.MatchString(userID) {
		return nil, fmt.Errorf("ejudge: invalid user id %q", userID)
	}
	short, err := c.resolveProblemShortName(ctx, problemID)
	if err != nil {
		return nil, err
	}
	// short comes from ejudge's own problem table, not from the caller.
	filter := fmt.Sprintf(`login==%q && prob==%q`, userID, short)
	body, err := c.masterGet(ctx, actionMainPage, url.Values{"filter_expr": {filter}})
	if err != nil {
		return nil, err
	}

	i := strings.Index(body, "<h2>Submissions</h2>")
	if i < 0 {
		return nil, fmt.Errorf("ejudge: submissions list: %w", ErrMalformedResponse)
	}
	var rows []masterSubmissionRow
	for _, m := range masterRowRe.FindAllStringSubmatch(body[i:], -1) {
		rows = append(rows, masterSubmissionRow{
			runID:    m[1],
			language: strings.TrimSpace(m[3]),
			result:   strings.TrimSpace(m[4]),
			failed:   strings.TrimSpace(m[5]),
		})
	}
	return rows, nil
}

type viewSourceInfo struct {
	code        string
	submittedAt time.Time
}

var (
	submissionTimeRe = regexp.MustCompile(`<td>(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})</td>`)
	sourceTableRe    = regexp.MustCompile(`(?s)<table class="b0"><tr><td valign="top" class="b0">.*?</td><td valign="top" class="b0">(.*?)</td></tr></table>`)
	sourceLineRe     = regexp.MustCompile(`(?s)<tt>(.*?)</tt>`)
)

func (c *Client) fetchViewSource(ctx context.Context, runID string) (viewSourceInfo, error) {
	body, err := c.masterGet(ctx, actionViewSource, url.Values{"run_id": {runID}})
	if err != nil {
		return viewSourceInfo{}, err
	}
	if hasErrorDetail(body, "is out of range") {
		return viewSourceInfo{}, fmt.Errorf("%w: run %s", ErrRunNotFound, runID)
	}

	tm := submissionTimeRe.FindStringSubmatch(body)
	if tm == nil {
		return viewSourceInfo{}, fmt.Errorf("ejudge: run %s source: %w", runID, ErrMalformedResponse)
	}
	submittedAt, err := time.ParseInLocation("2006/01/02 15:04:05", tm[1], time.Local)
	if err != nil {
		return viewSourceInfo{}, fmt.Errorf("ejudge: run %s submission time: %w", runID, err)
	}

	code := ""
	if sm := sourceTableRe.FindStringSubmatch(body); sm != nil {
		var lines []string
		for _, lm := range sourceLineRe.FindAllStringSubmatch(sm[1], -1) {
			lines = append(lines, decodeSourceLine(lm[1]))
		}
		code = strings.Join(lines, "\n")
	}

	return viewSourceInfo{code: code, submittedAt: submittedAt}, nil
}

func decodeSourceLine(line string) string {
	decoded := html.UnescapeString(line)
	return strings.ReplaceAll(decoded, " ", " ")
}

// fetchTestCounts derives (tests passed, tests total) from the run
// report's per-test table (action=37): count of "OK" rows, and the number
// of rows judged before ejudge stopped (see package doc for why the
// latter is a lower bound, not the problem's true test count, on a
// non-passing run). A run with no report yet (still queued/running)
// yields (0, 0), which callers treat like a compile-error attempt.
func (c *Client) fetchTestCounts(ctx context.Context, runID string) (passed, total int, err error) {
	body, err := c.masterGet(ctx, actionViewReport, url.Values{"run_id": {runID}})
	if err != nil {
		return 0, 0, err
	}
	if hasErrorTitle(body, "Report is not available") {
		return 0, 0, nil
	}
	// An error page has no test rows, so counting it would yield (0,0) —
	// indistinguishable from a compile error, which makes pick.Best discard
	// a perfectly good submission. Surface it instead.
	if isErrorPage(body) {
		return 0, 0, fmt.Errorf("ejudge: run %s report: %w", runID, ErrMalformedResponse)
	}
	passed, total = countReportTests(body)
	return passed, total, nil
}

var (
	reportVerdictRe = regexp.MustCompile(`<h2><font color="(?:red|green)">([^<]+)</font></h2>`)
	reportRowRe     = regexp.MustCompile(`<tr><td class="b1">(\d+)</td><td class="b1"><font color="(?:red|green)">([^<]+)</font></td>`)
)

// isErrorPage reports whether body is one of ejudge's error pages rather
// than a run report. Those pages render their message in exactly the same
// <h2><font color="red"> shape as a real verdict, so without this check a
// transient master error or a lost session reads back as a legitimately
// judged failing run — and the repair loop burns a retry on it instead of
// the request surfacing as infrastructure failure.
func isErrorPage(body string) bool {
	return strings.Contains(body, "Operation completed with errors") ||
		strings.Contains(body, "<h2><font color=\"red\">Error:") ||
		strings.Contains(body, "<h2><font color=\"red\">Permission denied</font></h2>")
}

var titleTagRe = regexp.MustCompile(`(?is)<title>(.*?)</title>`)

// hasErrorTitle reports whether ejudge's own <title> for this page carries
// msg.
//
// Every one of these sentinels used to be matched against the whole
// document, which is wrong on pages that embed text we do not control: a
// report page carries each test's stdout and stderr, and a view-source page
// carries the student's entire source. A submission that prints or merely
// comments "k is out of range" therefore reported itself as ErrRunNotFound
// — turning an ordinary failing verification into status=failed instead of
// no_fix, and collapsing exactly the split isErrorPage exists to protect.
// The student writes that text, so it is also forgeable on purpose.
//
// The <title> is generated by ejudge from its own message catalogue and
// never contains a submission's text, so it is the safe place to look.
func hasErrorTitle(body, msg string) bool {
	m := titleTagRe.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	return strings.Contains(m[1], msg)
}

// hasErrorDetail reports whether body is an ejudge error page whose detail
// block carries msg. Used for messages ejudge renders only in the
// "Additional information about this error" section (its title there is the
// generic "Operation completed with errors"), so the isErrorPage gate is
// what keeps the match off ordinary report content.
func hasErrorDetail(body, msg string) bool {
	return isErrorPage(body) && strings.Contains(body, msg)
}

func parseReportVerdict(body string) (string, error) {
	if isErrorPage(body) {
		return "", fmt.Errorf("ejudge: run report: %w", ErrMalformedResponse)
	}
	m := reportVerdictRe.FindStringSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("ejudge: run report: %w", ErrMalformedResponse)
	}
	return strings.TrimSpace(m[1]), nil
}

func countReportTests(body string) (passed, total int) {
	for _, m := range reportRowRe.FindAllStringSubmatch(body, -1) {
		total++
		if strings.TrimSpace(m[2]) == "OK" {
			passed++
		}
	}
	return passed, total
}

func reportTestVerdict(body string, testID int) (string, bool) {
	for _, m := range reportRowRe.FindAllStringSubmatch(body, -1) {
		if m[1] == strconv.Itoa(testID) {
			return strings.TrimSpace(m[2]), true
		}
	}
	return "", false
}

// reportTestDetail extracts test testID's Input/Output/Correct blocks from
// the report's "====== Test #N =======" section. ejudge delimits each
// section with a "size N" byte count immediately followed by exactly N
// bytes of (HTML-escaped) content, so this reads by declared length
// rather than by a delimiter regex — robust even when a test's content
// happens to contain "<a name=" or similar marker-shaped text.
func reportTestDetail(body string, testID int) (input, output, correct string, ok bool) {
	marker := fmt.Sprintf("====== Test #%d =======", testID)
	i := strings.Index(body, marker)
	if i < 0 {
		return "", "", "", false
	}
	section := body[i:]
	// Bound the section to just before the next test marker, if any.
	if next := strings.Index(section[len(marker):], "====== Test #"); next >= 0 {
		section = section[:len(marker)+next]
	}

	input, hasIn := readSizedField(section, "Input")
	output, hasOut := readSizedField(section, "Output")
	correctVal, hasCorrect := readSizedField(section, "Correct")
	if !hasIn && !hasOut && !hasCorrect {
		return "", "", "", false
	}
	return input, output, correctVal, true
}

var sizedFieldRe = regexp.MustCompile(`--- (\w+): size (\d+) ---</u>\n?`)

// readSizedField finds "--- label: size N ---</u>" in section and returns
// the field's N bytes of content.
//
// The declared size is a byte count of the *raw* file, but the page carries
// it HTML-escaped, where a single '<' occupies 4 bytes and '&' 5. Slicing
// the escaped text by the raw size therefore stops short — often mid-entity
// — on any test data containing those characters, which is exactly the
// input the repair model reasons about. So unescape first, then take size
// bytes.
func readSizedField(section, label string) (string, bool) {
	for _, m := range sizedFieldRe.FindAllStringSubmatchIndex(section, -1) {
		name := section[m[2]:m[3]]
		if name != label {
			continue
		}
		size, err := strconv.Atoi(section[m[4]:m[5]])
		if err != nil {
			return "", false
		}
		decoded := html.UnescapeString(section[m[1]:])
		if size > len(decoded) {
			size = len(decoded)
		}
		return decoded[:size], true
	}
	return "", false
}
