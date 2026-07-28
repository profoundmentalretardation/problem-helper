// Package platform defines the judging-platform abstraction the rest of the
// service depends on: problem statements/status, a user's submissions, and
// running code as a system user for repair-loop verification. ejudge is the
// first implementation (internal/platform/ejudge); internal/platform/mock is
// the scriptable test double every other package tests against.
package platform

import (
	"context"
	"time"
)

// Statement is a problem's statement, as pulled from the platform.
type Statement struct {
	ProblemID string
	Title     string
	Text      string
}

// Status reports whether a user has already solved a problem.
type Status struct {
	Solved bool
}

// Submission is one of a user's submissions to a problem.
type Submission struct {
	ID          string
	Code        string
	Language    string
	TestsPassed int
	TestsTotal  int
	SubmittedAt time.Time
}

// RunResult is the state of a run (a real submission or a system-user
// verification run). ID must be persisted by the caller before polling, so a
// crash-recovered worker resumes the existing run instead of resubmitting.
type RunResult struct {
	ID          string
	Done        bool
	Passed      bool
	TestsPassed int
	TestsTotal  int
}

// TestCase is one test's detail for a specific run — not for a problem in
// general, since hidden test inputs mean this can only be observed for tests
// that were actually executed as part of a run.
type TestCase struct {
	Index    int
	Input    string
	Expected string
	Actual   string
	Verdict  string
}

// Platform is the judging-platform abstraction. Every method is scoped to a
// single platform instance/course; problemID and userID are opaque strings
// from the platform's own namespace.
type Platform interface {
	ProblemStatement(ctx context.Context, problemID string) (Statement, error)
	ProblemStatus(ctx context.Context, userID, problemID string) (Status, error)
	Submissions(ctx context.Context, userID, problemID string, limit int) ([]Submission, error)
	SubmitAsSystem(ctx context.Context, problemID, code, lang string) (RunResult, error)
	RunResult(ctx context.Context, runID string) (RunResult, error)
	TestResult(ctx context.Context, runID string, testID int) (TestCase, error)
}
