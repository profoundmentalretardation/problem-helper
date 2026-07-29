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

	// ErrSubmitRejected is returned when ejudge refuses a submission for a
	// reason of its own that is neither a duplicate nor a permission problem —
	// the contest being over, a per-user submission limit, a disabled
	// language, a source file over the contest's size cap. Those render an
	// error page with no run table, so without this they came back as a bare
	// ErrMalformedResponse and the judge's stated reason was thrown away.
	ErrSubmitRejected = errors.New("ejudge: submission rejected")
)

const (
	defaultContestID = "1"
	defaultTimeout   = 30 * time.Second

	// maxResponseBytes caps a single ejudge response. Generous, because a
	// report page embeds every test's input and output and a source page
	// embeds a whole submission — but finite, because doOnce reads the body
	// before it knows whether the response is even ejudge's.
	maxResponseBytes = 32 << 20

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

	// Read off the <title>, not the whole document: the submit response
	// renders the problem statement and its samples, so a statement (or a
	// sample) carrying either phrase would turn a submit that actually
	// succeeded into ErrDuplicateSubmission — abandoning a real run and
	// burning a repair retry — or into ErrAuthFailed, failing the request.
	// Same reasoning as hasErrorTitle, which submitWithSession already
	// applies to this very response.
	if hasErrorTitle(body, "duplicate of another run") {
		return platform.RunResult{}, ErrDuplicateSubmission
	}
	if hasErrorTitle(body, "Permission denied") {
		return platform.RunResult{}, ErrAuthFailed
	}
	// Any *other* refusal — "The contest is over", a per-user submission limit,
	// a disabled language, a source file over the contest's size cap — renders
	// an error page with no "Previous submissions" section at all, so it used
	// to reach parseSubmitRunRows and come back as a bare ErrMalformedResponse
	// with the judge's own reason (which sits in the <title>) discarded. That
	// reports an internal fault for a judge that answered perfectly clearly,
	// and it is the same class of "the judge said no" as a duplicate: a
	// rejection, not our infrastructure breaking. It is checked after the two
	// specific sentinels above so those keep their own typed errors.
	if isErrorPage(body) {
		return platform.RunResult{}, fmt.Errorf("%w: %s", ErrSubmitRejected, submitRejectionReason(body))
	}

	// The run ids come from a table the whole service shares, since every
	// verification submits under one system login. Anything not newer than the
	// pre-submit page's top row belongs to some earlier submission, and polling
	// it would "verify" a run that never contained this code.
	fresh, err := parseRunRowsNewerThan(body, page.newestRunID)
	if err != nil {
		return platform.RunResult{}, err
	}
	switch len(fresh) {
	case 0:
		newest, _ := parseNewestRunID(body)
		return platform.RunResult{}, fmt.Errorf(
			"ejudge: submit returned run %s, not newer than pre-submit run %s: %w",
			newest, page.newestRunID, ErrMalformedResponse)
	case 1:
		// Exactly one run appeared while we were posting: it is ours, and no
		// extra request is needed to say so.
		return platform.RunResult{ID: fresh[0].id, Done: false}, nil
	}

	// More than one appeared, so "newest above the floor" is no longer an
	// identification: another instance (or another goroutine of this one)
	// submitted its own repair for the same problem under the same login in
	// the same window, and its run can perfectly well be on top. Taking it
	// would poll somebody else's code for this request's verdict, which is the
	// one guarantee loop 1 exists to provide — so the tie is broken on the
	// only thing that actually distinguishes the runs: their source.
	runID, err := c.pickRunBySource(ctx, fresh, code, lang)
	if err != nil {
		return platform.RunResult{}, err
	}
	return platform.RunResult{ID: runID, Done: false}, nil
}

// pickRunBySource returns the run among candidates that carries the code we
// just submitted, compiled with the language we submitted it under.
//
// The language is checked, not assumed: a run's verification identity is
// (source, compiler), and the one case where two concurrent runs can hold
// byte-identical source is precisely a differing language — ejudge's
// ignore_duplicated_runs rejects a repeat of the same source under the same
// login *and* language, so the surviving collision is `gcc` vs `g++` on the
// same file. Taking the newest matching source there would poll another
// request's run, under another compiler, for this one's verdict.
//
// Source comparison is whitespace-tolerant because the source page is markup,
// not the original bytes: it is rendered line by line, so trailing whitespace
// and the final newline do not survive the round trip.
//
// A row whose language the table did not report — parseSubmitRunRows leaves it
// empty when the contest's configuration renders no Language column — is still
// considered, because discarding it would throw away the only candidates there
// are. But source alone is not an identification in that case: the collision
// this function exists to break is by construction a same-source, differing-
// language one, so if two or more language-unknown runs carry the submitted
// code we have reproduced the original ambiguity and must fail closed rather
// than take the newest. A run whose language the table *did* report as ours is
// unambiguous and wins immediately.
func (c *Client) pickRunBySource(ctx context.Context, candidates []submitRunRow, code, lang string) (string, error) {
	want := normalizeSource(code)
	ids := make([]string, 0, len(candidates))
	for _, r := range candidates {
		ids = append(ids, r.id)
	}
	var unknownLang []string
	for _, r := range candidates {
		if r.language != "" && r.language != lang {
			continue
		}
		src, err := c.fetchViewSource(ctx, r.id)
		if err != nil {
			return "", fmt.Errorf("ejudge: identifying our run among %v: %w", ids, err)
		}
		if normalizeSource(src.code) != want {
			continue
		}
		if r.language == lang {
			return r.id, nil
		}
		unknownLang = append(unknownLang, r.id)
	}
	switch len(unknownLang) {
	case 0:
		return "", fmt.Errorf(
			"ejudge: submit produced runs %v under the shared system login and none carries the submitted code in %s: %w",
			ids, lang, ErrMalformedResponse)
	case 1:
		return unknownLang[0], nil
	default:
		return "", fmt.Errorf(
			"ejudge: submit produced runs %v under the shared system login carrying the submitted code, and the table reports no language to tell them apart: %w",
			unknownLang, ErrMalformedResponse)
	}
}

// normalizeSource strips the differences ejudge's own rendering introduces —
// CRLFs, trailing whitespace on a line, and trailing blank lines — so two
// spellings of the same submission compare equal.
func normalizeSource(code string) string {
	lines := strings.Split(strings.ReplaceAll(code, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
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
		// ErrRunNotFound only — wrapping ErrMalformedResponse alongside it
		// made one error satisfy both errors.Is checks, collapsing the
		// outcome-vs-infrastructure-fault split the failed/no_fix
		// distinction rests on.
		return platform.RunResult{}, fmt.Errorf("%w: run %s", ErrRunNotFound, runID)
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
	// Same sentinels RunResult reads, for the same reason: this is a report
	// parser, and an error page renders in the same shape as a report. Without
	// them an unknown run id or a master-side error came back as an untyped
	// "has no test N", which satisfies neither errors.Is(ErrRunNotFound) nor
	// errors.Is(ErrMalformedResponse) — so the caller cannot tell a missing
	// run from a judge outage.
	if hasErrorDetail(body, "is out of range") {
		return platform.TestCase{}, fmt.Errorf("%w: run %s", ErrRunNotFound, runID)
	}
	if isErrorPage(body) {
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
	// Keyed on ejudge's own <title>, like every other sentinel: a successful
	// login response *is* the full main page, which renders participant
	// display names and problem titles. A bare body-wide match therefore let
	// any of that text turn a good login into ErrAuthFailed — i.e. every
	// request in the service failing authentication on correct credentials.
	if hasErrorTitle(body, "Permission denied") {
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
	return c.doWithRetry(req, true)
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("ejudge: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.doWithRetry(req, true)
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
	// A submit is the one request in this package that must never be sent
	// twice: a replay creates a second run under the shared system login.
	return c.doWithRetry(req, false)
}

// doWithRetry retries a couple of times with a short backoff.
//
// replayable says whether sending this request twice is safe. It is a
// property of the *request*, not of the failure: a submit POST creates a
// run under the shared system login, and c.httpClient.Timeout firing while
// awaiting response headers — the normal case for a judge that has already
// queued the run — is indistinguishable from a request that never arrived.
// Classifying retryability per-failure therefore replayed exactly the case
// it was written to prevent, so submits pass replayable=false and are never
// resent. Idempotent GETs pass true and do retry a 5xx/short read: ejudge
// answers its own errors with a 200, so those come from a proxy or CGI in
// front of it and are the transient failures that actually occur.
//
// A POST body is a one-shot reader: after the first attempt it is drained
// and closed, so every retry rebuilds the body from req.GetBody (which
// http.NewRequest populates for the in-memory body types this package
// uses). A request whose body cannot be replayed is not retried — resending
// it would silently submit an empty form.
func (c *Client) doWithRetry(req *http.Request, replayable bool) (string, error) {
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
			// Not safe to send twice — see the doc comment. One attempt is
			// all this request gets, whatever the failure looked like.
			if !replayable {
				return "", fmt.Errorf("ejudge: request to %s: %w", req.URL.Path, err)
			}
			continue // retry
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
		return "", redactURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded: report pages embed every test's stdin/stdout and source pages
	// embed a whole submission, so the responses are legitimately large — but
	// an unbounded ReadAll on a chunked response that never ends takes the
	// process down, and it runs before the status check below, so even a proxy
	// error page is buffered in full. Over the limit is an error rather than a
	// truncation: a truncated report under-counts TestsPassed, and pick.Best
	// then chooses on it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("%w: %s returned more than %d bytes",
			ErrMalformedResponse, req.URL.Path, maxResponseBytes)
	}
	// ejudge signals all of its own error conditions with a 200 and an error
	// page, so a 4xx/5xx here is something in front of it — an nginx 502, a
	// CGI 500. isErrorPage does not recognise those, so without this check a
	// proxy error page flows on into the parsers: countReportTests finds no
	// rows and reports (0, 0) with no error, every submission looks like a
	// compile error, and pick.Best chooses on garbage.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: %s returned %s", ErrMalformedResponse, req.URL.Path, resp.Status)
	}
	return string(data), nil
}

// redactURLError strips the query string from a transport error.
//
// Every request carries SID — an Administrator session token — and the
// master queries carry a filter_expr naming a student's login. net/http
// returns transport failures as *url.Error, whose Error() embeds the full
// URL, and that string is persisted verbatim on help_requests.error and
// served by GET /admin/requests. Only the path is ever useful for
// diagnosis, so the query never leaves this function.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	path := ue.URL
	if u, parseErr := url.Parse(ue.URL); parseErr == nil {
		path = u.Path
	}
	return fmt.Errorf("ejudge: %s %s: %w", ue.Op, path, ue.Err)
}

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
	if hasErrorTitle(body, "Invalid contest") || hasErrorTitle(body, "Invalid problem") {
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
//
// The regex is anchored on the row opener rather than matching any numeric
// cell: Size and Failed test are numbers too, so an unanchored scan reads the
// first row's size as the second row's run id — harmless while only the first
// match was ever used, wrong the moment a caller wants the whole column.
var (
	submitRunRowRe  = regexp.MustCompile(`(?s)<tr>\s*(<td class="b1">.*?)</tr>`)
	submitRunCellRe = regexp.MustCompile(`(?s)<td class="b1">(.*?)</td>`)
	submitRunHeadRe = regexp.MustCompile(`(?s)<th class="b1">(.*?)</th>`)
	runIDRe         = regexp.MustCompile(`^\d+$`)
)

// submitRunRow is one row of the post-submit "Previous submissions" table:
// the run id and the language ejudge reports for it. The language is part of
// a run's identity, not decoration — see pickRunBySource.
type submitRunRow struct {
	id       string
	language string
}

// parseSubmitRunRows reads the per-problem "Previous submissions" table
// rendered after a successful submit, newest row first.
//
// Rows are matched from the row opener rather than by scanning for any
// numeric cell: Size and Failed test are numbers too, so an unanchored scan
// reads the first row's size as the second row's run id. The Language column
// is located by its header rather than by a fixed index, because the columns
// this table renders depend on the contest's configuration; if there is no
// such header the language is simply left empty and callers fall back to
// identifying a run by its source alone.
func parseSubmitRunRows(body string) ([]submitRunRow, error) {
	i := strings.Index(body, "Previous submissions of this problem")
	if i < 0 {
		return nil, fmt.Errorf("ejudge: submit response: %w", ErrMalformedResponse)
	}
	section := body[i:]
	if end := strings.Index(section, "</table>"); end >= 0 {
		section = section[:end]
	}

	langCol := -1
	for k, m := range submitRunHeadRe.FindAllStringSubmatch(section, -1) {
		if strings.EqualFold(strings.TrimSpace(htmlToText(m[1])), "Language") {
			langCol = k
			break
		}
	}

	var rows []submitRunRow
	for _, m := range submitRunRowRe.FindAllStringSubmatch(section, -1) {
		cells := submitRunCellRe.FindAllStringSubmatch(m[1], -1)
		if len(cells) == 0 {
			continue
		}
		id := strings.TrimSpace(cells[0][1])
		if !runIDRe.MatchString(id) {
			continue
		}
		row := submitRunRow{id: id}
		if langCol >= 0 && langCol < len(cells) {
			row.language = strings.TrimSpace(htmlToText(cells[langCol][1]))
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ejudge: submit response: %w", ErrMalformedResponse)
	}
	return rows, nil
}

func parseNewestRunID(body string) (string, error) {
	rows, err := parseSubmitRunRows(body)
	if err != nil {
		return "", err
	}
	return rows[0].id, nil
}

// parseRunRowsNewerThan returns every run in the post-submit table that is
// newer than floor, newest first — the candidates for "the run this submit
// created" — each with the language ejudge reports for it.
func parseRunRowsNewerThan(body, floor string) ([]submitRunRow, error) {
	rows, err := parseSubmitRunRows(body)
	if err != nil {
		return nil, err
	}
	var fresh []submitRunRow
	for _, r := range rows {
		if isNewerRunID(r.id, floor) {
			fresh = append(fresh, r)
		}
	}
	return fresh, nil
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

	// A source page whose code table we cannot parse is a malformed response,
	// not an empty submission. Returning code="" successfully sent the repair
	// model, the hint model and the judge a program the student never wrote —
	// silently, because the timestamp above still parsed, so nothing else on
	// the pipeline had any reason to doubt the submission. ejudge renders even
	// a zero-byte source as the table with no <tt> lines in it, so "the table
	// is missing" and "the table is empty" are both markup we no longer
	// recognise.
	sm := sourceTableRe.FindStringSubmatch(body)
	if sm == nil {
		return viewSourceInfo{}, fmt.Errorf("ejudge: run %s source table: %w", runID, ErrMalformedResponse)
	}
	lineMatches := sourceLineRe.FindAllStringSubmatch(sm[1], -1)
	if len(lineMatches) == 0 {
		return viewSourceInfo{}, fmt.Errorf("ejudge: run %s source is empty: %w", runID, ErrMalformedResponse)
	}
	lines := make([]string, 0, len(lineMatches))
	for _, lm := range lineMatches {
		lines = append(lines, decodeSourceLine(lm[1]))
	}

	return viewSourceInfo{code: strings.Join(lines, "\n"), submittedAt: submittedAt}, nil
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
	// The first two predicates key on ejudge's own generated markup. A bare
	// substring test for the plain phrase "Operation completed with errors"
	// used to lead this chain, but report pages embed each test's stdout and
	// stderr and source pages embed the whole submission — so a student
	// could print that phrase and, since hasErrorDetail gates on this
	// function, forge ErrRunNotFound/ErrMalformedResponse out of an ordinary
	// failing run. Both real error fixtures match on the markup instead.
	//
	// The two markup predicates match the *client*-role error shape. Every
	// masterGet lands on a master-role page, whose error documents render as
	// a plain <h2>Operation completed with errors</h2> / <h2>Permission
	// denied</h2> with no <font color>, so a master-only error (no embedded
	// client document) matched neither — fetchTestCounts fell through to
	// countReportTests and answered (0, 0), the silent compile-error
	// lookalike this gate exists to prevent, and hasErrorDetail stopped
	// producing ErrRunNotFound. The <title> is the safe place to look for
	// those, for the reason hasErrorTitle documents.
	return strings.Contains(body, "<h2><font color=\"red\">Error:") ||
		strings.Contains(body, "<h2><font color=\"red\">Permission denied</font></h2>") ||
		hasErrorTitle(body, "Operation completed with errors") ||
		hasErrorTitle(body, "Permission denied")
}

var titleTagRe = regexp.MustCompile(`(?is)<title>(.*?)</title>`)

// submitRejectionReason extracts a human-readable reason from an ejudge error
// page. It reads the <title>s and nothing else, for the reason hasErrorTitle
// documents: the rest of the submit response renders the problem statement and
// its samples, which the course's problem authors write, and this string is
// persisted on help_requests.error and served by GET /admin/requests.
func submitRejectionReason(body string) string {
	var titles []string
	for _, m := range titleTagRe.FindAllStringSubmatch(body, -1) {
		if t := strings.TrimSpace(htmlToText(m[1])); t != "" {
			titles = append(titles, t)
		}
	}
	if len(titles) == 0 {
		return "no reason given"
	}
	return strings.Join(titles, "; ")
}

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
// Every <title> on the page is checked, not just the first. Master-wrapped
// error pages carry two — the outer master document's ("… : Operation
// completed with errors") and the embedded client document's ("… : Error:
// …"), as run_report_out_of_range.html and run_report_server_error.html both
// show. Reading only the first meant the specific sentinel was missed and the
// page fell through to the generic isErrorPage path: a run merely sitting in
// the judge queue became ErrMalformedResponse — status=failed — instead of the
// "not available yet" RunResult promises, and an expired session was never
// recognised for the re-login it needs.
func hasErrorTitle(body, msg string) bool {
	for _, m := range titleTagRe.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], msg) {
			return true
		}
	}
	return false
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
	// Anchored on ejudge's own <b>…</b> markup, never on the bare text. The
	// report embeds each test's stdout HTML-escaped, and this marker contains
	// no HTML-special characters, so a program that simply prints
	// "====== Test #2 =======" reproduces it byte-for-byte inside test 1's
	// Output block — a student-forged anchor that made TestResult(run, 2)
	// read test 1's fields and hand the repair model test data for a test it
	// was not diagnosing. Same rule as hasErrorTitle/isErrorPage: sentinels
	// come off markup the judge generates, not off text the submission
	// controls.
	marker := fmt.Sprintf("<b>====== Test #%d =======</b>", testID)
	i := strings.Index(body, marker)
	if i < 0 {
		return "", "", "", false
	}
	section := body[i:]
	// Bound the section to just before the next test marker, if any.
	if next := strings.Index(section[len(marker):], "<b>====== Test #"); next >= 0 {
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
