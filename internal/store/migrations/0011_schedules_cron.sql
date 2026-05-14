-- PR-17 / roadmap E41 + E42: cron expressions + one-off scheduled tasks.
--
-- Two new schedule kinds:
--   - "cron":    full 5-field cron expression in `cron_expr`.
--   - "oneoff":  fires exactly once at next_run_at, then auto-disables.
--
-- We also add a `cron_expr` column for the cron kind; oneoff reuses
-- the existing `next_run_at`.
ALTER TABLE schedules ADD COLUMN cron_expr TEXT NOT NULL DEFAULT '';
