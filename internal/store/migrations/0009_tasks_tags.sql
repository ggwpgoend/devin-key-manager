-- PR-15: AI/search helpers (K79 prompt suggestions, K81 embedding-search,
-- K82 auto-tag, L89 token estimator).
--
-- 1. tasks.tags — same comma-separated format as keys.tags from PR-12.
--    Powers auto-tag suggestions and tag-based filtering on the tasks page.
--    Lowercase, no spaces; e.g. "bug,refactor".
ALTER TABLE tasks ADD COLUMN tags TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_tasks_tags ON tasks(tags);
