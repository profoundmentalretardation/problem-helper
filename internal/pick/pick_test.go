package pick_test

import (
	"errors"
	"testing"
	"time"

	"github.com/profoundmentalretardation/problem-helper/internal/pick"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
)

func sub(id string, testsPassed, testsTotal int, submittedAt time.Time) platform.Submission {
	return platform.Submission{
		ID:          id,
		TestsPassed: testsPassed,
		TestsTotal:  testsTotal,
		SubmittedAt: submittedAt,
	}
}

func TestBest_MaxTestsPassedWins(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	subs := []platform.Submission{
		sub("a", 3, 10, t0),
		sub("b", 7, 10, t0.Add(time.Minute)),
		sub("c", 5, 10, t0.Add(2*time.Minute)),
	}

	got, err := pick.Best(subs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("Best() = %q, want %q", got.ID, "b")
	}
}

func TestBest_TieBrokenByLatest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	subs := []platform.Submission{
		sub("older", 5, 10, t0),
		sub("newer", 5, 10, t0.Add(time.Hour)),
	}

	got, err := pick.Best(subs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "newer" {
		t.Errorf("Best() = %q, want %q", got.ID, "newer")
	}
}

func TestBest_EmptyList(t *testing.T) {
	_, err := pick.Best(nil)
	if !errors.Is(err, pick.ErrNoSubmissions) {
		t.Fatalf("err = %v, want ErrNoSubmissions", err)
	}
}

func TestBest_AllCompileErrorsOnly(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	subs := []platform.Submission{
		sub("a", 0, 0, t0),
		sub("b", 0, 0, t0.Add(time.Minute)),
	}

	_, err := pick.Best(subs)
	if !errors.Is(err, pick.ErrNoSubmissions) {
		t.Fatalf("err = %v, want ErrNoSubmissions", err)
	}
}

func TestBest_IgnoresCompileErrorSubmissionsAmongOthers(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	subs := []platform.Submission{
		sub("compile-error", 0, 0, t0.Add(time.Hour)), // latest, but unusable
		sub("real", 2, 10, t0),
	}

	got, err := pick.Best(subs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "real" {
		t.Errorf("Best() = %q, want %q", got.ID, "real")
	}
}
