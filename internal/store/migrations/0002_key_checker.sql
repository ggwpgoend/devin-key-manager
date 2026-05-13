-- Adds bookkeeping columns for the key validity checker.
-- last_checked_at: when the checker last pinged Devin with this key.
-- last_check_status: short tag (valid, unauthorized, quota_exhausted, rate_limited, network_error).
-- last_check_error: free-form error message from the most recent failed check.

ALTER TABLE keys ADD COLUMN last_checked_at TIMESTAMP;
ALTER TABLE keys ADD COLUMN last_check_status TEXT NOT NULL DEFAULT '';
ALTER TABLE keys ADD COLUMN last_check_error TEXT NOT NULL DEFAULT '';
