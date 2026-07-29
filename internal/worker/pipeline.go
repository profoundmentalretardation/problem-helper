// pipeline.go implements the request pipeline (steps 1-9 of the plan's
// diagram) as a function of a claimed (status=running) help_requests row:
// platform status/statement/submissions, best-submission pick, shield,
// hint-cache lookup, the repair loop, the hint loop, and delivery.
// resume_step is checkpointed after each step for Task 14's crash-reclaim
// to resume against; branching on an existing resume_step to skip completed
// work is Task 14's scope, not this one's.
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
	SetBestSubmission(ctx context.Context, id, submissionID uuid.UUID) error
	SetHintID(ctx context.Context, id, hintID uuid.UUID) error
	SetFailureReason(ctx context.Context, id uuid.UUID, reason string) error
	SetError(ctx context.Context, id uuid.UUID, message string) error
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

	statement, err := pl.Platform.ProblemStatement(ctx, hr.ProblemID)
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("fetching problem statement: %w", err))
	}
	if err := pl.checkpoint(ctx, requestID, StepStatement); err != nil {
		return err
	}

	subs, err := pl.Platform.Submissions(ctx, hr.UserID, hr.ProblemID, hr.NSubmissionsTaken)
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("fetching submissions: %w", err))
	}

	best, err := pick.Best(subs)
	if errors.Is(err, pick.ErrNoSubmissions) {
		if _, serr := pl.snapshotSubmissions(ctx, requestID, subs, ""); serr != nil {
			return serr
		}
		return pl.finish(ctx, requestID, store.StatusNoSubmissions)
	}
	if err != nil {
		return pl.infraFail(ctx, requestID, fmt.Errorf("picking best submission: %w", err))
	}

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

	shielded, err := shield.Strip(best.Code, best.Language)
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
	if err := pl.checkpoint(ctx, requestID, StepShield); err != nil {
		return err
	}

	codeHash := HashCode(shielded.CodeAfter)
	if cached, ok := Lookup(ctx, pl.Store, hr.ProblemID, codeHash); ok {
		if err := pl.Store.SetHintID(ctx, requestID, cached.ID); err != nil {
			return fmt.Errorf("pipeline: recording cached hint id: %w", err)
		}
		if err := pl.event(ctx, requestID, "hint_cache_hit", map[string]any{"hint_id": cached.ID}); err != nil {
			return err
		}
		return pl.finish(ctx, requestID, store.StatusDone)
	}
	if err := pl.checkpoint(ctx, requestID, StepCache); err != nil {
		return err
	}

	repairResult, err := pl.Repair.Run(ctx, repair.Params{
		RequestID:          requestID,
		UserID:             hr.UserID,
		ProblemID:          hr.ProblemID,
		Language:           best.Language,
		ProblemStatement:   statement.Text,
		UserCode:           shielded.CodeAfter,
		BaselineRunID:      best.ID,
		BaselineTestsTotal: best.TestsTotal,
	})
	if err != nil {
		return fmt.Errorf("pipeline: running repair loop: %w", err)
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
		return fmt.Errorf("pipeline: running hint loop: %w", err)
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
