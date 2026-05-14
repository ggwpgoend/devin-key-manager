-- Per-session free-form notes (PR-5).
--
-- The user can scribble whatever they want here (reminders why this session
-- was opened, what to try next, links to upstream issues, …). These notes
-- never leave the manager; they are not sent to Devin.

ALTER TABLE sessions ADD COLUMN notes TEXT NOT NULL DEFAULT '';
