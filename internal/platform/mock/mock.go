// Package mock is a scriptable fake implementing platform.Platform, the test
// double every other package's tests run against until Task 15 wires up the
// real ejudge client. Every call must be scripted in advance; an unscripted
// call panics so a test fails loudly instead of silently getting a zero
// value. NewDefaulting relaxes that for unscripted reads — see its doc
// comment — and is for the PLATFORM=mock binary, not for tests.
package mock

import (
	"context"
	"fmt"

	"github.com/profoundmentalretardation/problem-helper/internal/platform"
)

type pairKey struct {
	userID, problemID string
}

type testKey struct {
	runID  string
	testID int
}

// submitItem is one queued response for SubmitAsSystem — either a result or
// an error, never both.
type submitItem struct {
	result platform.RunResult
	err    error
}

// Platform is the scriptable fake. Zero value is not usable; construct with
// New.
type Platform struct {
	statements  map[string]platform.Statement
	statuses    map[pairKey]platform.Status
	statusErrs  map[pairKey]error
	submissions map[pairKey][]platform.Submission
	submitQueue map[string][]submitItem
	runResults  map[string]platform.RunResult
	// runResultQueue holds sequenced RunResult answers for a run id; it
	// takes precedence over runResults so a test can script a poll sequence
	// for a run that SubmitAsSystem also registered.
	runResultQueue map[string][]platform.RunResult
	testCases      map[testKey]platform.TestCase
	// answerDefaults makes unscripted *read* calls return a benign zero-ish
	// answer instead of panicking. Off for tests (an unscripted call there is
	// a test bug that must fail loudly); on for the PLATFORM=mock binary,
	// where nothing is scripted at all.
	answerDefaults bool
}

// New returns an empty scriptable mock.
func New() *Platform {
	return newPlatform()
}

// NewDefaulting returns a mock that answers unscripted reads with benign
// defaults rather than panicking: no statement text, problem unsolved, no
// submissions. It exists for PLATFORM=mock, the local/dev backend documented
// in the README — with the panicking New, the very first pipeline step
// panicked, runPipelineRecovered turned that into a failed step, and the row
// cycled through every reclaim until maxClaimAttempts marked it failed. With
// defaults the pipeline instead terminates cleanly at no_submissions, and a
// caller that wants a full run scripts the data explicitly.
func NewDefaulting() *Platform {
	p := newPlatform()
	p.answerDefaults = true
	return p
}

func newPlatform() *Platform {
	return &Platform{
		statements:     map[string]platform.Statement{},
		statuses:       map[pairKey]platform.Status{},
		statusErrs:     map[pairKey]error{},
		submissions:    map[pairKey][]platform.Submission{},
		submitQueue:    map[string][]submitItem{},
		runResults:     map[string]platform.RunResult{},
		runResultQueue: map[string][]platform.RunResult{},
		testCases:      map[testKey]platform.TestCase{},
	}
}

// ScriptStatement scripts the result of ProblemStatement(problemID).
func (p *Platform) ScriptStatement(problemID string, s platform.Statement) {
	p.statements[problemID] = s
}

// ScriptStatus scripts the result of ProblemStatus(userID, problemID).
func (p *Platform) ScriptStatus(userID, problemID string, s platform.Status) {
	p.statuses[pairKey{userID, problemID}] = s
}

// ScriptStatusError scripts ProblemStatus(userID, problemID) to fail with
// err instead of returning a status — for tests exercising a platform
// outage.
func (p *Platform) ScriptStatusError(userID, problemID string, err error) {
	p.statusErrs[pairKey{userID, problemID}] = err
}

// ScriptSubmissions scripts the result of Submissions(userID, problemID, _);
// the limit argument truncates this slice at call time.
func (p *Platform) ScriptSubmissions(userID, problemID string, subs []platform.Submission) {
	p.submissions[pairKey{userID, problemID}] = subs
}

// ScriptSubmitResult appends a RunResult to the queue consumed, in order, by
// successive SubmitAsSystem(problemID, ...) calls. The result is also made
// pollable via RunResult(result.ID) immediately.
func (p *Platform) ScriptSubmitResult(problemID string, r platform.RunResult) {
	p.submitQueue[problemID] = append(p.submitQueue[problemID], submitItem{result: r})
}

// ScriptSubmitError appends err to the same ordered queue as
// ScriptSubmitResult, so a specific SubmitAsSystem call in a multi-attempt
// sequence can be made to fail — for tests exercising a platform outage
// mid-repair-loop.
func (p *Platform) ScriptSubmitError(problemID string, err error) {
	p.submitQueue[problemID] = append(p.submitQueue[problemID], submitItem{err: err})
}

// ScriptRunResult scripts the result of RunResult(runID) directly, without
// going through SubmitAsSystem — for tests that resume polling an
// already-submitted run.
func (p *Platform) ScriptRunResult(runID string, r platform.RunResult) {
	p.runResults[runID] = r
}

// ScriptRunResultSequence scripts consecutive RunResult(runID) answers, so a
// test can drive a caller's polling loop through "not done yet" states. The
// last entry is sticky: once the sequence is exhausted every further poll
// returns it, which keeps a never-done sequence (a run wedged in the judge
// queue) from panicking as an unscripted call.
func (p *Platform) ScriptRunResultSequence(runID string, results ...platform.RunResult) {
	if len(results) == 0 {
		panic("mock: ScriptRunResultSequence needs at least one result")
	}
	p.runResultQueue[runID] = results
}

// ScriptTestCase scripts the result of TestResult(runID, testID).
func (p *Platform) ScriptTestCase(runID string, testID int, tc platform.TestCase) {
	p.testCases[testKey{runID, testID}] = tc
}

func (p *Platform) ProblemStatement(_ context.Context, problemID string) (platform.Statement, error) {
	s, ok := p.statements[problemID]
	if !ok {
		if p.answerDefaults {
			return platform.Statement{ProblemID: problemID}, nil
		}
		panic(fmt.Sprintf("mock: unscripted ProblemStatement(%q)", problemID))
	}
	return s, nil
}

func (p *Platform) ProblemStatus(_ context.Context, userID, problemID string) (platform.Status, error) {
	if err, ok := p.statusErrs[pairKey{userID, problemID}]; ok {
		return platform.Status{}, err
	}
	s, ok := p.statuses[pairKey{userID, problemID}]
	if !ok {
		if p.answerDefaults {
			return platform.Status{}, nil
		}
		panic(fmt.Sprintf("mock: unscripted ProblemStatus(%q, %q)", userID, problemID))
	}
	return s, nil
}

func (p *Platform) Submissions(_ context.Context, userID, problemID string, limit int) ([]platform.Submission, error) {
	subs, ok := p.submissions[pairKey{userID, problemID}]
	if !ok {
		if p.answerDefaults {
			return nil, nil
		}
		panic(fmt.Sprintf("mock: unscripted Submissions(%q, %q)", userID, problemID))
	}
	if limit > 0 && limit < len(subs) {
		subs = subs[:limit]
	}
	return subs, nil
}

func (p *Platform) SubmitAsSystem(_ context.Context, problemID, _, _ string) (platform.RunResult, error) {
	q := p.submitQueue[problemID]
	if len(q) == 0 {
		panic(fmt.Sprintf("mock: unscripted SubmitAsSystem(%q)", problemID))
	}
	item := q[0]
	p.submitQueue[problemID] = q[1:]
	if item.err != nil {
		return platform.RunResult{}, item.err
	}
	p.runResults[item.result.ID] = item.result
	return item.result, nil
}

func (p *Platform) RunResult(_ context.Context, runID string) (platform.RunResult, error) {
	if q := p.runResultQueue[runID]; len(q) > 0 {
		r := q[0]
		if len(q) > 1 {
			p.runResultQueue[runID] = q[1:]
		}
		return r, nil
	}
	r, ok := p.runResults[runID]
	if !ok {
		panic(fmt.Sprintf("mock: unscripted RunResult(%q)", runID))
	}
	return r, nil
}

func (p *Platform) TestResult(_ context.Context, runID string, testID int) (platform.TestCase, error) {
	tc, ok := p.testCases[testKey{runID, testID}]
	if !ok {
		panic(fmt.Sprintf("mock: unscripted TestResult(%q, %d)", runID, testID))
	}
	return tc, nil
}
