-- Bound how many times one help request may be claimed. A request that
-- crashes or errors deterministically (a panic, a store failure, a corrupt
-- checkpoint) leaves its row in 'running' with no terminal status, so the
-- reclaim sweep returns it to 'pending' forever — and because the repair and
-- hint loops are not resume-guarded, every cycle re-spends the model budget
-- and re-submits to the judge. claim_attempts lets ReclaimStale give up.

ALTER TABLE help_requests ADD COLUMN claim_attempts INT NOT NULL DEFAULT 0;
