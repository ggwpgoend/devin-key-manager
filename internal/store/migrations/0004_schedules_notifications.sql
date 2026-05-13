-- PR-8: scheduled tasks + browser notification events.
--
-- schedules:        recurring task triggers. A small scheduler goroutine
--                   scans this table once per tick, fires anything whose
--                   next_run_at has passed, and bumps next_run_at forward.
--                   Two trigger kinds are supported:
--                     - 'interval': every interval_seconds seconds since
--                       last_run_at.
--                     - 'daily':    every day at daily_hour:daily_minute
--                       in the server's local time.
--                   plan_hint is an optional preferred key plan ('trial',
--                   'free', 'paid', '') — if blank the picker uses its
--                   normal trial-first order.
--
-- notification_events:
--                   append-only log surfaced to the browser by the
--                   /events/since endpoint. The frontend uses the Web
--                   Notification API to pop a toast for each new event.
--                   We deliberately keep this independent from sessions /
--                   tasks so deleting a task does not orphan history; the
--                   related_*_id columns are best-effort backlinks.

CREATE TABLE IF NOT EXISTS schedules (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL,
    prompt          TEXT NOT NULL,
    plan_hint       TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,             -- 'interval' or 'daily'
    interval_seconds INTEGER NOT NULL DEFAULT 0,
    daily_hour      INTEGER NOT NULL DEFAULT 0,
    daily_minute    INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    next_run_at     TIMESTAMP NOT NULL,
    last_run_at     TIMESTAMP,
    last_session_id TEXT,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_schedules_due
    ON schedules(enabled, next_run_at);

CREATE TABLE IF NOT EXISTS notification_events (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                TEXT NOT NULL,         -- 'devin_message' | 'handoff' | 'quota' | 'schedule_fired' | 'system'
    title               TEXT NOT NULL,
    body                TEXT NOT NULL DEFAULT '',
    url                 TEXT NOT NULL DEFAULT '',
    related_task_id     TEXT,
    related_session_id  TEXT,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_notifications_created
    ON notification_events(created_at DESC);
