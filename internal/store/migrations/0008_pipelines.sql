-- PR-13 E43.1: pipeline editor (n8n-style DAG).
--
-- A "pipeline" is a directed graph the user composes in the browser.
-- Each node represents an action; each edge represents control flow
-- (optionally guarded by a condition label like "true" / "false" / "default").
--
-- Layout: positions (x,y) are persisted so re-loading shows the canvas
-- exactly as the user arranged it. Configurations are JSON blobs scoped to
-- the node type so the schema can evolve per node type without altering
-- the rows table.

CREATE TABLE IF NOT EXISTS pipelines (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- A monotonically increasing version. Bumped each time the user saves
    -- the canvas, so old runs can still be associated with the snapshot
    -- they were started against.
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);

-- Nodes live in a separate table so the editor can re-layout / re-render
-- quickly without rewriting the entire pipeline row.
CREATE TABLE IF NOT EXISTS pipeline_nodes (
    id          TEXT PRIMARY KEY,
    pipeline_id TEXT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    type        TEXT NOT NULL,                -- 'trigger','prompt','wait','condition','handoff','notify','end'
    label       TEXT NOT NULL DEFAULT '',     -- human-readable display name
    pos_x       REAL NOT NULL DEFAULT 0,
    pos_y       REAL NOT NULL DEFAULT 0,
    -- Per-node-type config; JSON blob. Examples:
    --   prompt:    {"prompt": "...", "session_strategy": "new" | "current"}
    --   wait:      {"strategy": "idle" | "duration", "duration_sec": 60}
    --   condition: {"left": "last_message.contains", "right": "ok", "op": "eq"}
    --   handoff:   {"reason": "manual rotate"}
    --   notify:    {"channel": "browser", "message": "Pipeline X done."}
    config_json TEXT NOT NULL DEFAULT '{}',
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_pipeline_nodes_pipeline ON pipeline_nodes(pipeline_id);

CREATE TABLE IF NOT EXISTS pipeline_edges (
    id          TEXT PRIMARY KEY,
    pipeline_id TEXT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    source_id   TEXT NOT NULL REFERENCES pipeline_nodes(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES pipeline_nodes(id) ON DELETE CASCADE,
    -- Guard label. For conditional nodes, edges are labelled 'true' / 'false'.
    -- Otherwise 'default'. The runtime walks edges whose condition matches
    -- the node's result.
    condition   TEXT NOT NULL DEFAULT 'default',
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_pipeline_edges_pipeline ON pipeline_edges(pipeline_id);
CREATE INDEX IF NOT EXISTS idx_pipeline_edges_source ON pipeline_edges(source_id);

-- One row per pipeline-run. Bookkeeping for "rollback one level" (the user
-- can rewind to any prior step the runner visited).
CREATE TABLE IF NOT EXISTS pipeline_runs (
    id              TEXT PRIMARY KEY,
    pipeline_id     TEXT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    pipeline_version INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending,running,succeeded,failed,cancelled
    current_node_id TEXT,                            -- where the runner is (or last was) in the graph
    started_at      TIMESTAMP NOT NULL,
    ended_at        TIMESTAMP,
    error_msg       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_pipeline_runs_pipeline ON pipeline_runs(pipeline_id);

-- Append-only audit trail of every node the runner visited, in order.
-- "Rollback one level" pops the most recent entry and re-points the run
-- at the previous step.
CREATE TABLE IF NOT EXISTS pipeline_run_steps (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL REFERENCES pipeline_nodes(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,           -- monotonic ordering within a run
    status      TEXT NOT NULL DEFAULT 'pending', -- pending,running,succeeded,failed,skipped
    started_at  TIMESTAMP NOT NULL,
    ended_at    TIMESTAMP,
    -- Outputs (JSON), useful for "what did the condition evaluate to?".
    output_json TEXT NOT NULL DEFAULT '{}',
    error_msg   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_pipeline_run_steps_run ON pipeline_run_steps(run_id, seq);
