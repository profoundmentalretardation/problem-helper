package mock_test

import (
	"context"
	"testing"
	"time"

	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/mock"
)

func TestProblemStatement_Scripted(t *testing.T) {
	p := mock.New()
	p.ScriptStatement("prob-1", platform.Statement{ProblemID: "prob-1", Title: "Sum", Text: "add two numbers"})

	got, err := p.ProblemStatement(context.Background(), "prob-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Title != "Sum" {
		t.Errorf("Title = %q, want %q", got.Title, "Sum")
	}
}

func TestProblemStatement_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted ProblemStatement call")
		}
	}()
	_, _ = p.ProblemStatement(context.Background(), "unknown")
}

func TestProblemStatus_Scripted(t *testing.T) {
	p := mock.New()
	p.ScriptStatus("user-1", "prob-1", platform.Status{Solved: true})

	got, err := p.ProblemStatus(context.Background(), "user-1", "prob-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Solved {
		t.Errorf("Solved = false, want true")
	}
}

func TestProblemStatus_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted ProblemStatus call")
		}
	}()
	_, _ = p.ProblemStatus(context.Background(), "user-1", "prob-1")
}

func TestSubmissions_Scripted_AndLimit(t *testing.T) {
	p := mock.New()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.ScriptSubmissions("user-1", "prob-1", []platform.Submission{
		{ID: "s1", Language: "go", TestsPassed: 1, TestsTotal: 10, SubmittedAt: t0},
		{ID: "s2", Language: "go", TestsPassed: 2, TestsTotal: 10, SubmittedAt: t0.Add(time.Minute)},
	})

	got, err := p.Submissions(context.Background(), "user-1", "prob-1", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Errorf("Submissions(limit=1) = %+v, want [s1]", got)
	}
}

func TestSubmissions_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted Submissions call")
		}
	}()
	_, _ = p.Submissions(context.Background(), "user-1", "prob-1", 10)
}

func TestSubmitAsSystem_ScriptedQueue_ThenRunResultPollable(t *testing.T) {
	p := mock.New()
	p.ScriptSubmitResult("prob-1", platform.RunResult{ID: "run-1", Done: true, Passed: true, TestsPassed: 10, TestsTotal: 10})

	got, err := p.SubmitAsSystem(context.Background(), "prob-1", "code", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "run-1" || !got.Passed {
		t.Errorf("SubmitAsSystem() = %+v", got)
	}

	polled, err := p.RunResult(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("unexpected error polling: %v", err)
	}
	if polled != got {
		t.Errorf("RunResult(run-1) = %+v, want %+v", polled, got)
	}
}

func TestSubmitAsSystem_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted SubmitAsSystem call")
		}
	}()
	_, _ = p.SubmitAsSystem(context.Background(), "prob-1", "code", "go")
}

func TestRunResult_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted RunResult call")
		}
	}()
	_, _ = p.RunResult(context.Background(), "unknown-run")
}

func TestTestResult_Scripted(t *testing.T) {
	p := mock.New()
	p.ScriptTestCase("run-1", 3, platform.TestCase{Index: 3, Input: "2 3", Expected: "5", Actual: "5", Verdict: "OK"})

	got, err := p.TestResult(context.Background(), "run-1", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Expected != "5" {
		t.Errorf("Expected = %q, want %q", got.Expected, "5")
	}
}

func TestTestResult_Unscripted_Panics(t *testing.T) {
	p := mock.New()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unscripted TestResult call")
		}
	}()
	_, _ = p.TestResult(context.Background(), "run-1", 1)
}

func TestSubmitAsSystem_QueueIsConsumedInOrder(t *testing.T) {
	p := mock.New()
	p.ScriptSubmitResult("prob-1", platform.RunResult{ID: "run-1", Done: true, Passed: false})
	p.ScriptSubmitResult("prob-1", platform.RunResult{ID: "run-2", Done: true, Passed: true})

	first, err := p.SubmitAsSystem(context.Background(), "prob-1", "code-v1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.ID != "run-1" {
		t.Errorf("first submit = %q, want run-1", first.ID)
	}

	second, err := p.SubmitAsSystem(context.Background(), "prob-1", "code-v2", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.ID != "run-2" {
		t.Errorf("second submit = %q, want run-2", second.ID)
	}
}

var _ platform.Platform = (*mock.Platform)(nil)
