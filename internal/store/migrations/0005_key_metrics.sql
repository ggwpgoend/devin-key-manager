-- PR-10: per-key usage metrics.
--
-- These columns are append-only counters / last-write fields read by the
-- dashboard. They live on the keys table so the per-row read in
-- /keys is still a single SELECT (no JOIN).
--
-- request_count          : total Devin API calls made on behalf of this key.
--                          Incremented on every successful client call.
-- last_error_message     : the most recent error string seen on this key.
--                          Truncated to 500 chars when written.
-- last_error_at          : when that error happened.
-- activated_at           : when this key was first used (FIRST request).
--                          NULL until then; lets the UI show "Activated 3d ago".
-- sessions_count_total   : count of distinct sessions ever opened on this key.
ALTER TABLE keys ADD COLUMN request_count        INTEGER   NOT NULL DEFAULT 0;
ALTER TABLE keys ADD COLUMN last_error_message   TEXT      NOT NULL DEFAULT '';
ALTER TABLE keys ADD COLUMN last_error_at        TIMESTAMP;
ALTER TABLE keys ADD COLUMN activated_at         TIMESTAMP;
ALTER TABLE keys ADD COLUMN sessions_count_total INTEGER   NOT NULL DEFAULT 0;
