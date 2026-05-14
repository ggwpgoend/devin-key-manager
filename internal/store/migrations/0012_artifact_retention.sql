-- PR-17 / roadmap D40: retention policy for artifacts.
--
-- Adds a `pinned` flag on artifacts so the retention sweep can spare
-- the ones the user explicitly wants to keep. Default 0 — historical
-- artifacts behave the same as before.
ALTER TABLE artifacts ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS artifacts_retention ON artifacts(pinned, created_at);
