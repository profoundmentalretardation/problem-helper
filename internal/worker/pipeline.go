// pipeline.go implements the request pipeline (steps 1-9 of the plan's
// diagram) as a function of a claimed (status=running) help_requests row:
// platform status/statement/submissions, best-submission pick, shield,
// hint-cache lookup, the repair loop, the hint loop, and delivery.
// resume_step is checkpointed after each step; a reclaimed row (Task 14)
// resumes at its last checkpoint instead of restarting from step 1 —
// RunPipeline reloads whatever that step already persisted (best
// submission, shield record) from the store instead of re-deriving it, so
// steps that mutate the store (snapshotting submissions, inserting the
// shield record) never run twice for the same request.
//
// The repair and hint loops (steps 7-8) are not further subdivided: per the
// plan's "Resume granularity" decision, attempt-level checkpointing inside
// a loop is post-MVP, so a crash mid-loop re-enters the whole loop on
// resume (the loop's own baseline run is never re-submitted — only new
// verification attempts are, same as a non-resumed run).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/hint"
	"github.com/profoundmentalretardation/problem-helper/internal/agent/repair"
	"github.com/profoundmentalretardation/problem-helper/internal/pick"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/shield"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// Resume-step checkpoints, written after the step of the same name
// completes. Order matches the pipeline diagram.
const (
	StepStatus      = "status"
	StepStatement   = "statement"
	StepSubmissions = "submissions"
	StepShield      = "shield"
	StepCache       = "cache"
	StepRepair      = "repair"
	StepHint        = "hint"
)

// stepOrder ranks the checkpoints in pipeline order, so resumeIndex can
// tell "already past this step" from a stored resume_step. 0 is reserved
// for "no checkpoint yet" (a fresh request, ResumeStep == nil).
var stepOrder = map[string]int{
	StepStatus:      1,
	StepStatement:   2,
	StepSubmissions: 3,
	StepShield:      4,
	StepCache:       5,
	StepRepair:      6,
	StepHint:        7,
}

// resumeIndex returns step's position in stepOrder, or 0 if step is nil —
// a fresh request that hasn't completed any pipeline step yet.
func resumeIndex(step *string) int {
	if step == nil {
		return 0
	}
	return stepOrder[*step]
}

// Store is the persistence dependency the pipeline needs; *store.Store
// satisfies it.
type Store interface {
	GetHelpRequest(ctx context.Context, id uuid.UUID) (*store.HelpRequest, error)
	TransitionStatus(ctx context.Context, id uuid.UUID, to store.Status) error
	AppendEvent(ctx context.Context, id uuid.UUID, kind string, payload []byte) error
	SnapshotSubmissions(ctx context.Context, requestID uuid.UUID, subs []store.Submission) error
	InsertShieldRecord(ctx context.Context, r store.ShieldRecord) error
	FindApprovedHint(ctx context.Context, problemID, codeHash string) (*store.Hint, error)
	InsertHint(ctx context.Context, h store.Hint) error
	SetResumeStep(ctx context.Context, id uuid.UUID, step string) error
	GetSubmission(ctx context.Context, id uuid.UUID) (*store.Submission, error)
	GetShieldRecordByRequest(ctx context.Context, requestID uuid.UUID) (*store.ShieldRecord, error)
	SetBestSubmission(ctx context.Context, id, submissionID uuid.UUID) error
	SetHintID(ctx context.Context, id, hintID uuid.UUID) error
	SetFailureReason(ctx context.Context, id uuid.UUID, reason string) error
	SetError(ctx context.Context, id uuid.UUID, message string) error
	TopMistakes(ctx context.Context, userID string, limit int) ([]store.Mistake, error)
}

// RepairRunner runs loop 1; *repair.Runner satisfies it.
type RepairRunner interface {
	Run(ctx context.Context, p repair.Params) (repair.Result, error)
}

// HintRunner runs loop 2; *hint.Runner satisfies it.
type HintRunner interface {
	Run(ctx context.Context, p hint.Params) (hint.Result, error)
}

// Pipeline holds every dependency RunPipeline needs.
type Pipeline struct {
	Store    Store
	Platform platform.Platform
	Repair   RepairRunner
	Hint     HintRunner

	// TopNMistakes caps how many of the student's curated mistakes are fed
	// into the repair prompt (agents.yaml repair.top_n_mistakes). Zero
	// disables the lookup, so the prompt renders no mistakes at all.
	TopNMistakes int
}

// RunPipeline drives one help_requests row (already claimed, status=running)
// through steps 1-9, checkpointing resume_step after each and leaving the
// row in a terminal status. A returned error means bookkeeping itself
// failed (a Store call errored); every other outcome — solved, no
// submissions, an unsupported language, a repair/hint loop giving up, a
// dead platform — is recorded on the row as its own terminal status and
// RunPipeline returns nil.
func (pl *Pipeline) RunPipeline(ctx context.Context, requestID uuid.UUID) error {
	hr, err := pl.Store.GetHelpRequest(ctx, requestID)
	if err != nil {
		return fmt.Errorf("pipeline: loading request: %w", err)
	}
	resumeIdx := resumeIndex(hr.ResumeStep)

	// A row only reaches "running" with a checkpoint past StepStatus if an
	// earlier run of this same request already found it unsolved (a solved
	// problem stops the pipeline in a terminal status, never reaching this
	// checkpoint) — so a resumed run skips the recheck instead of spending
	// another platform call and duplicate event on it.
	if resumeIdx < stepOrder[StepStatus] {
		status, err := pl.Platform.ProblemStatus(ctx, hr.UserID, hr.ProblemID)
		if err != nil {
			return pl.infraFail(ctx, requestID, fmt.Errorf("checking problem status: %w", err))
		}
		if err := pl.event(ctx, requestID, "problem_status", map[string]any{"solved": status.Solved}); err != nil {
			return err
		}
		if status.Solved {
			return pl.finish(ctx, requestID, store.StatusAlreadySolved)
		}
		if err := pl.checkpoint(ctx, requestID, StepStatus); err != nil {
			return err
		}
	}

	// The problem statement is never persisted (only the repair loop needs
	// it), so it's re-fetched on every run regardless of resume — a cheap,
	// idempotent read with no store side effect to duplicate.
	statement, err := pl.Platform.ProblemStatement(ctx, hr.ProblemID)
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("fetching problem statement: %w", err))
	}
	if resumeIdx < stepOrder[StepStatement] {
		if err := pl.event(ctx, requestID, "problem_statement", map[string]any{"problem_id": statement.ProblemID}); err != nil {
			return err
		}
		if err := pl.checkpoint(ctx, requestID, StepStatement); err != nil {
			return err
		}
	}

	var best platform.Submission
	if resumeIdx < stepOrder[StepSubmissions] {
		subs, err := pl.Platform.Submissions(ctx, hr.UserID, hr.ProblemID, hr.NSubmissionsTaken)
		if err != nil {
			return pl.infraFail(ctx, requestID, fmt.Errorf("fetching submissions: %w", err))
		}

		picked, err := pick.Best(subs)
		if errors.Is(err, pick.ErrNoSubmissions) {
			if _, serr := pl.snapshotSubmissions(ctx, requestID, subs, ""); serr != nil {
				return serr
			}
			return pl.finish(ctx, requestID, store.StatusNoSubmissions)
		}
		if err != nil {
			return pl.infraFail(ctx, requestID, fmt.Errorf("picking best submission: %w", err))
		}
		best = picked

		bestStoreID, err := pl.snapshotSubmissions(ctx, requestID, subs, best.ID)
		if err != nil {
			return err
		}
		if err := pl.Store.SetBestSubmission(ctx, requestID, bestStoreID); err != nil {
			return fmt.Errorf("pipeline: recording best submission: %w", err)
		}
		if err := pl.event(ctx, requestID, "best_submission_picked", map[string]any{
			"platform_submission_id": best.ID, "tests_passed": best.TestsPassed, "tests_total": best.TestsTotal,
		}); err != nil {
			return err
		}
		if err := pl.checkpoint(ctx, requestID, StepSubmissions); err != nil {
			return err
		}
	} else {
		// Already snapshotted and picked before the last checkpoint;
		// reload rather than re-fetching (which would duplicate the
		// snapshot rows) or re-picking (the platform's submission list may
		// have changed since).
		if hr.BestSubmissionID == nil {
			return fmt.Errorf("pipeline: resuming past %s checkpoint with no best_submission_id recorded", StepSubmissions)
		}
		saved, err := pl.Store.GetSubmission(ctx, *hr.BestSubmissionID)
		if err != nil {
			return fmt.Errorf("pipeline: reloading best submission on resume: %w", err)
		}
		best = platform.Submission{
			ID:          saved.PlatformSubmissionID,
			Code:        saved.Code,
			Language:    saved.Language,
			TestsPassed: saved.TestsPassed,
			TestsTotal:  saved.TestsTotal,
			SubmittedAt: saved.SubmittedAt,
		}
	}

	var shielded shield.Result
	if resumeIdx < stepOrder[StepShield] {
		shielded, err = shield.Strip(best.Code, best.Language)
		if errors.Is(err, shield.ErrUnsupportedLanguage) {
			return pl.finishWithError(ctx, requestID, fmt.Sprintf("unsupported submission language %q", best.Language))
		}
		if err != nil {
			return pl.infraFail(ctx, requestID, fmt.Errorf("shielding submission: %w", err))
		}
		removed, err := json.Marshal(shielded.Removed)
		if err != nil {
			return fmt.Errorf("pipeline: encoding shield removal report: %w", err)
		}
		if err := pl.Store.InsertShieldRecord(ctx, store.ShieldRecord{
			RequestID:  requestID,
			CodeBefore: shielded.CodeBefore,
			CodeAfter:  shielded.CodeAfter,
			Diff:       shielded.Diff,
			Removed:    removed,
		}); err != nil {
			return fmt.Errorf("pipeline: recording shield record: %w", err)
		}
		if err := pl.event(ctx, requestID, "shield_applied", map[string]any{"removed": shielded.Removed}); err != nil {
			return err
		}
		if err := pl.checkpoint(ctx, requestID, StepShield); err != nil {
			return err
		}
	} else {
		// Already shielded and recorded before the last checkpoint; reload
		// instead of re-stripping (which would insert a duplicate record).
		rec, err := pl.Store.GetShieldRecordByRequest(ctx, requestID)
		if err != nil {
			return fmt.Errorf("pipeline: reloading shield record on resume: %w", err)
		}
		shielded = shield.Result{CodeBefore: rec.CodeBefore, CodeAfter: rec.CodeAfter, Diff: rec.Diff}
	}

	codeHash := HashCode(shielded.CodeAfter)
	if resumeIdx < stepOrder[StepCache] {
		if cached, ok := Lookup(ctx, pl.Store, hr.ProblemID, codeHash); ok {
			if err := pl.Store.SetHintID(ctx, requestID, cached.ID); err != nil {
				return fmt.Errorf("pipeline: recording cached hint id: %w", err)
			}
			if err := pl.event(ctx, requestID, "hint_cache_hit", map[string]any{"hint_id": cached.ID}); err != nil {
				return err
			}
			// A cache re-delivery is still a delivery: HintEffectivenessInputs
			// keys off hint_delivered, so without this the student's hint is
			// invisible to the effectiveness analytics.
			if err := pl.event(ctx, requestID, "hint_delivered", map[string]any{"hint_id": cached.ID, "cached": true}); err != nil {
				return err
			}
			return pl.finish(ctx, requestID, store.StatusDone)
		}
		if err := pl.checkpoint(ctx, requestID, StepCache); err != nil {
			return err
		}
	}
	// resumeIdx >= StepCache means this request already missed the cache
	// before its last checkpoint — a hit would have finished the pipeline
	// terminally, never leaving a later checkpoint to resume from.

	// The curated per-student mistake profile the nightly metaloop builds is
	// only worth anything if it comes back into a prompt — this is the read
	// side of that loop.
	mistakes, err := pl.topMistakes(ctx, hr.UserID)
	if err != nil {
		return pl.infraFail(ctx, requestID, err)
	}

	repairResult, err := pl.Repair.Run(ctx, repair.Params{
		RequestID:          requestID,
		UserID:             hr.UserID,
		ProblemID:          hr.ProblemID,
		Language:           best.Language,
		ProblemStatement:   statement.Text,
		UserCode:           shielded.CodeAfter,
		Mistakes:           mistakes,
		BaselineRunID:      best.ID,
		BaselineTestsTotal: best.TestsTotal,
	})
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("running repair loop: %w", err))
	}
	if err := pl.event(ctx, requestID, "repair_result", map[string]any{
		"status": repairResult.Status, "reason": repairResult.Reason, "attempts": repairResult.Attempts,
	}); err != nil {
		return err
	}
	if repairResult.Status == repair.StatusNoFix {
		return pl.finishWithReason(ctx, requestID, store.StatusNoFix, string(repairResult.Reason))
	}
	if err := pl.checkpoint(ctx, requestID, StepRepair); err != nil {
		return err
	}

	hintResult, err := pl.Hint.Run(ctx, hint.Params{
		RequestID:    requestID,
		OriginalCode: shielded.CodeAfter,
		WorkingCode:  repairResult.Code,
	})
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("running hint loop: %w", err))
	}
	if err := pl.event(ctx, requestID, "hint_result", map[string]any{
		"status": hintResult.Status, "reason": hintResult.Reason, "attempts": hintResult.Attempts,
	}); err != nil {
		return err
	}
	if hintResult.Status == hint.StatusNoHint {
		return pl.finishWithReason(ctx, requestID, store.StatusNoHint, string(hintResult.Reason))
	}
	if err := pl.checkpoint(ctx, requestID, StepHint); err != nil {
		return err
	}

	hintID := uuid.New()
	if err := pl.Store.InsertHint(ctx, store.Hint{
		ID:        hintID,
		RequestID: requestID,
		ProblemID: hr.ProblemID,
		CodeHash:  codeHash,
		Text:      hintResult.Hint,
		Approved:  true,
	}); err != nil {
		return fmt.Errorf("pipeline: inserting hint: %w", err)
	}
	if err := pl.Store.SetHintID(ctx, requestID, hintID); err != nil {
		return fmt.Errorf("pipeline: recording delivered hint id: %w", err)
	}
	if err := pl.event(ctx, requestID, "hint_delivered", map[string]any{"hint_id": hintID}); err != nil {
		return err
	}
	return pl.finish(ctx, requestID, store.StatusDone)
}

// topMistakes loads the student's curated mistake profile, rendered one
// mistake per line for the repair prompt's {{mistakes}} placeholder.
func (pl *Pipeline) topMistakes(ctx context.Context, userID string) ([]string, error) {
	if pl.TopNMistakes <= 0 {
		return nil, nil
	}
	rows, err := pl.Store.TopMistakes(ctx, userID, pl.TopNMistakes)
	if err != nil {
		return nil, fmt.Errorf("loading top mistakes: %w", err)
	}
	lines := make([]string, len(rows))
	for i, m := range rows {
		lines[i] = fmt.Sprintf("- %s: %s (seen %d times)", m.Title, m.Description, m.Count)
	}
	return lines, nil
}

// snapshotSubmissions converts and stores every pulled submission, marking
// the one whose platform id matches bestPlatformID (if any) as best, and
// returns that submission's store-generated id.
func (pl *Pipeline) snapshotSubmissions(ctx context.Context, requestID uuid.UUID, subs []platform.Submission, bestPlatformID string) (uuid.UUID, error) {
	rows := make([]store.Submission, len(subs))
	var bestStoreID uuid.UUID
	for i, sub := range subs {
		id := uuid.New()
		isBest := bestPlatformID != "" && sub.ID == bestPlatformID
		if isBest {
			bestStoreID = id
		}
		rows[i] = store.Submission{
			ID:                   id,
			PlatformSubmissionID: sub.ID,
			Code:                 sub.Code,
			Language:             sub.Language,
			TestsPassed:          sub.TestsPassed,
			TestsTotal:           sub.TestsTotal,
			SubmittedAt:          sub.SubmittedAt,
			IsBest:               isBest,
		}
	}
	if err := pl.Store.SnapshotSubmissions(ctx, requestID, rows); err != nil {
		return uuid.Nil, fmt.Errorf("pipeline: snapshotting submissions: %w", err)
	}
	return bestStoreID, nil
}

// checkpoint records the resume_step for requestID after a step completes.
func (pl *Pipeline) checkpoint(ctx context.Context, requestID uuid.UUID, step string) error {
	if err := pl.Store.SetResumeStep(ctx, requestID, step); err != nil {
		return fmt.Errorf("pipeline: checkpointing step %q: %w", step, err)
	}
	return nil
}

// event marshals payload and appends it as an events row.
func (pl *Pipeline) event(ctx context.Context, requestID uuid.UUID, kind string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pipeline: encoding %s event: %w", kind, err)
	}
	if err := pl.Store.AppendEvent(ctx, requestID, kind, data); err != nil {
		return fmt.Errorf("pipeline: recording %s event: %w", kind, err)
	}
	return nil
}

// finish transitions requestID to a terminal status reached without error
// (already_solved, no_submissions, done).
func (pl *Pipeline) finish(ctx context.Context, requestID uuid.UUID, status store.Status) error {
	if err := pl.Store.TransitionStatus(ctx, requestID, status); err != nil {
		return fmt.Errorf("pipeline: transitioning to %s: %w", status, err)
	}
	return nil
}

// finishWithReason records failure_reason then transitions to a declined
// terminal status (no_fix, no_hint) — our infra didn't break, we chose not
// to deliver.
func (pl *Pipeline) finishWithReason(ctx context.Context, requestID uuid.UUID, status store.Status, reason string) error {
	if err := pl.Store.SetFailureReason(ctx, requestID, reason); err != nil {
		return fmt.Errorf("pipeline: recording failure reason: %w", err)
	}
	return pl.finish(ctx, requestID, status)
}

// finishWithError records error then transitions to status=failed for a
// clear, non-retryable problem detected without a lower-level error (e.g. an
// unsupported submission language).
func (pl *Pipeline) finishWithError(ctx context.Context, requestID uuid.UUID, message string) error {
	if err := pl.Store.SetError(ctx, requestID, message); err != nil {
		return fmt.Errorf("pipeline: recording error: %w", err)
	}
	return pl.finish(ctx, requestID, store.StatusFailed)
}

// infraFail records err's message then transitions requestID to
// status=failed for an infrastructure/platform error encountered mid-step.
func (pl *Pipeline) infraFail(ctx context.Context, requestID uuid.UUID, err error) error {
	if serr := pl.Store.SetError(ctx, requestID, err.Error()); serr != nil {
		return fmt.Errorf("pipeline: recording error for %v: %w", err, serr)
	}
	return pl.finish(ctx, requestID, store.StatusFailed)
}
