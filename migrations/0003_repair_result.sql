-- Persist the repair loop's outcome so the "repair" resume checkpoint is
-- actually usable. Without it, resume_step='repair' (or 'hint') could only
-- be written, never honoured: the working code the hint loop needs lived in
-- memory, so a reclaimed row re-entered loop 1 from scratch — re-spending
-- both model budgets and submitting to the judge again under the system
-- login. repair_run_id is the verified run the fix was accepted on, kept for
-- the same reason the plan requires the run id persisted before polling.

ALTER TABLE help_requests ADD COLUMN repair_code TEXT;
ALTER TABLE help_requests ADD COLUMN repair_run_id TEXT;
