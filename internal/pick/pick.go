// Package pick selects the best submission to feed into the repair loop.
package pick

import (
	"errors"

	"github.com/profoundmentalretardation/problem-helper/internal/platform"
)

// ErrNoSubmissions is returned when there is no usable submission: an empty
// list, or every submission has TestsTotal == 0 (compile errors only, never
// actually run against tests).
var ErrNoSubmissions = errors.New("pick: no usable submissions")

// Best picks the best submission: max tests passed wins, ties broken by the
// most recently submitted. Submissions with TestsTotal == 0 are excluded as
// unusable (compile errors, not real attempts).
func Best(subs []platform.Submission) (platform.Submission, error) {
	var best platform.Submission
	found := false

	for _, s := range subs {
		if s.TestsTotal == 0 {
			continue
		}
		if !found {
			best = s
			found = true
			continue
		}
		if s.TestsPassed > best.TestsPassed ||
			(s.TestsPassed == best.TestsPassed && s.SubmittedAt.After(best.SubmittedAt)) {
			best = s
		}
	}

	if !found {
		return platform.Submission{}, ErrNoSubmissions
	}
	return best, nil
}
