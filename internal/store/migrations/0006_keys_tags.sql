-- PR-12: per-key tags + pinned messages + message FTS5 search.
--
-- 1. keys.tags is a comma-separated list of lowercase tags. We keep this
--    inline rather than a join table because:
--      - the UI filter is "match-any" not relational,
--      - tag cardinality is small (dozens at most),
--      - the same row needs to be selected for the keys table anyway, so a
--        single column read is cheaper than a JOIN.
--    Format: "tag1,tag2,tag3" (no leading/trailing comma, no spaces).
ALTER TABLE keys ADD COLUMN tags TEXT NOT NULL DEFAULT '';

-- 2. messages.pinned + messages.pinned_at let the chat UI surface
--    important messages at the top of the session view (C22).
ALTER TABLE messages ADD COLUMN pinned     INTEGER   NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN pinned_at  TIMESTAMP;

-- 3. messages_fts is an SQLite FTS5 virtual table over messages.content,
--    keyed by messages.id. The chat search (C29) and the cross-session
--    "find when Devin helped me with X" (K81 partial) query against this.
--    We populate it via triggers so the FTS index never drifts from the
--    canonical messages table.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='rowid',
    tokenize = "unicode61 remove_diacritics 2"
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- Backfill existing rows so historical messages are searchable too.
INSERT INTO messages_fts(rowid, content)
SELECT rowid, content FROM messages
WHERE rowid NOT IN (SELECT rowid FROM messages_fts);

-- 4. session_attachments stores files uploaded by the user that are then
--    relayed to Devin via a public URL (C26). The link is shareable so the
--    Devin worker can fetch from anywhere; we keep the row for audit +
--    re-share without re-uploading.
CREATE TABLE IF NOT EXISTS session_attachments (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    filename    TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    mime_type   TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL DEFAULT 'fileio',
    public_url  TEXT NOT NULL DEFAULT '',
    local_path  TEXT NOT NULL DEFAULT '',
    error_msg   TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMP NOT NULL,
    uploaded_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS session_attachments_session ON session_attachments(session_id, created_at DESC);
