-- Two integrity gaps the snapshot step left open.
--
-- 1. submissions had no uniqueness constraint, and SnapshotSubmissions
--    inserted row-by-row outside a transaction. A crash between the inserts
--    and the StepSubmissions checkpoint left a partial snapshot committed;
--    the reclaimed run then re-inserted *every* submission with fresh ids,
--    up to store.maxClaimAttempts times. That yields duplicate rows, more
--    than one is_best=true, and an inflated submission count in
--    HintEffectivenessInputs (whose COUNT(DISTINCT sub.id) defends against
--    the two LEFT JOINs fanning each other out, not against real duplicates).
--    The unique index lets the insert be idempotent via ON CONFLICT.
--
-- 2. The daily rate-limit query filters help_requests on
--    (user_id, created_at) and ran on every POST /help with only
--    help_requests_status_idx to work with, i.e. a sequential scan that
--    grows with the whole table.

-- The duplicates described above are, by construction, already present on
-- any database that ran the old code — so creating the unique index first
-- would raise 23505, Migrate would return it, and main.go would abort at
-- startup on every boot with no way forward. Clear them first, keeping the
-- oldest row of each group (the one the first, uninterrupted pass wrote).
DELETE FROM submissions a
      USING submissions b
      WHERE a.request_id = b.request_id
        AND a.platform_submission_id = b.platform_submission_id
        AND a.ctid > b.ctid;

-- best_submission_id has no foreign key, so a row abandoned by the delete
-- above (or by the pre-fix random-id resume) can point at nothing. Null
-- those out rather than leaving GetSubmission to error the pipeline: the
-- submissions rows are still there, and re-picking is cheap.
UPDATE help_requests
   SET best_submission_id = NULL
 WHERE best_submission_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM submissions s WHERE s.id = help_requests.best_submission_id);

CREATE UNIQUE INDEX submissions_request_platform_id_idx
    ON submissions (request_id, platform_submission_id);

CREATE INDEX help_requests_user_created_idx
    ON help_requests (user_id, created_at);
