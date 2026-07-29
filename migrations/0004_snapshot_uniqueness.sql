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

CREATE UNIQUE INDEX submissions_request_platform_id_idx
    ON submissions (request_id, platform_submission_id);

CREATE INDEX help_requests_user_created_idx
    ON help_requests (user_id, created_at);
