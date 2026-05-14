-- PR-12 B14: session checkpoints (fork).
--
-- A "checkpoint" is an explicit user-initiated fork point on a session:
-- a snapshot of all messages up to the chosen anchor message, copied
-- verbatim onto a freshly-created session whose forked_from_session_id
-- points back at the original. This lets the user explore "what if I'd
-- told Devin X here?" without losing the current branch.
--
-- We do NOT model checkpoints as their own table; the fork itself is
-- the artifact. The columns below merely let the UI show breadcrumbs
-- like "branched from <other session> at <anchor>".

ALTER TABLE sessions ADD COLUMN forked_from_session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN forked_from_message_id TEXT;
ALTER TABLE sessions ADD COLUMN forked_at TIMESTAMP;

CREATE INDEX IF NOT EXISTS idx_sessions_forked_from ON sessions(forked_from_session_id) WHERE forked_from_session_id IS NOT NULL;
