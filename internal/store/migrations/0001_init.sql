-- Initial schema for devin-key-manager.
-- All tables use TEXT primary keys (ULIDs/UUIDs) for stability across exports.

CREATE TABLE IF NOT EXISTS keys (
    id                          TEXT PRIMARY KEY,
    label                       TEXT NOT NULL,
    plan_type                   TEXT NOT NULL CHECK (plan_type IN ('trial', 'free', 'paid')),
    api_key_encrypted           TEXT NOT NULL,
    api_key_fingerprint         TEXT NOT NULL,
    state                       TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'cooldown_daily', 'cooldown_weekly', 'dead')),
    cooldown_until              TIMESTAMP,
    daily_cycles_used_this_week INTEGER NOT NULL DEFAULT 0,
    week_reset_at               TIMESTAMP,
    last_used_at                TIMESTAMP,
    notes                       TEXT NOT NULL DEFAULT '',
    created_at                  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_keys_fingerprint ON keys(api_key_fingerprint);
CREATE INDEX IF NOT EXISTS idx_keys_state ON keys(state);

-- Logical task created by the user. A task may span multiple sessions across
-- multiple keys as quotas are exhausted and handoffs are performed.
CREATE TABLE IF NOT EXISTS tasks (
    id               TEXT PRIMARY KEY,
    title            TEXT NOT NULL,
    initial_prompt   TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'paused', 'completed', 'failed', 'cancelled')),
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

-- One row per Devin session created on behalf of a task. Each session uses
-- exactly one key and produces a stream of messages plus optional artifacts.
CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT PRIMARY KEY,
    task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    key_id            TEXT NOT NULL REFERENCES keys(id),
    devin_session_id  TEXT,
    status            TEXT NOT NULL DEFAULT 'creating' CHECK (status IN ('creating', 'running', 'blocked', 'completed', 'failed', 'quota_exhausted', 'handoff_pending', 'handoff_done')),
    started_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ended_at          TIMESTAMP,
    end_reason        TEXT,
    last_polled_at    TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_task ON sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);

CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
    content     TEXT NOT NULL,
    ts          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, ts);

CREATE TABLE IF NOT EXISTS artifacts (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id  TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    filename    TEXT NOT NULL,
    local_path  TEXT NOT NULL,
    devin_url   TEXT,
    sha256      TEXT,
    size_bytes  INTEGER,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_artifacts_task ON artifacts(task_id);

CREATE TABLE IF NOT EXISTS handoffs (
    id               TEXT PRIMARY KEY,
    task_id          TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    from_session_id  TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    to_session_id    TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    markdown         TEXT NOT NULL,
    attachments_json TEXT NOT NULL DEFAULT '[]',
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_handoffs_task ON handoffs(task_id);
