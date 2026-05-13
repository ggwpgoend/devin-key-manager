-- Extends the artifacts table for PR-4 (files & inline previews).
--
-- content_type: MIME type as reported by the remote server. Drives inline
--               rendering choices (images render as <img>, everything else
--               renders as a download link).
-- source:       who produced the attachment — devin or the local user.
-- status:       download lifecycle. The poller writes pending rows the moment
--               it spots a URL; a background goroutine flips them to ready
--               (or failed) once the file has been streamed to disk.
-- error:        human-readable description for failed downloads.
--
-- The (session_id, devin_url) unique index lets the poller de-duplicate URLs
-- so we don't re-download the same file every time the chat re-syncs.

ALTER TABLE artifacts ADD COLUMN content_type TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN source TEXT NOT NULL DEFAULT 'devin';
ALTER TABLE artifacts ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE artifacts ADD COLUMN error TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_artifacts_session ON artifacts(session_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_session_url
    ON artifacts(session_id, devin_url) WHERE devin_url IS NOT NULL;
