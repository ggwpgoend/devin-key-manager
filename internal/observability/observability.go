// Package observability provides time-series aggregates and graph builders
// for the dashboard's "Observability" tab (PR-14).
//
// The repo intentionally returns plain Go structs, not pre-rendered HTML —
// the web layer reshapes these into JSON for the (vanilla-JS) charts and
// the session-graph SVG. Keeping the queries here means we can unit-test
// them and call the same code from the JSON API without going through the
// template layer.
package observability

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Repo runs the time-series + graph queries against the shared SQLite DB.
type Repo struct {
	db DB
}

// DB is the tiny subset of *store.DB / *sql.DB we need. Both implementations
// expose QueryContext, so unit tests can pass a thin fake if desired.
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewRepo wires the repo around an existing DB.
func NewRepo(db DB) *Repo { return &Repo{db: db} }

// BucketSize selects the rollup granularity for time-series queries.
type BucketSize string

const (
	BucketHour BucketSize = "hour"
	BucketDay  BucketSize = "day"
)

// TimePoint is one bucket of a time series.
type TimePoint struct {
	BucketStart time.Time `json:"bucket_start"`
	Count       int       `json:"count"`
}

// Series is a labelled time series ready for charting.
type Series struct {
	Name   string      `json:"name"`
	Points []TimePoint `json:"points"`
}

// strftimeFormat returns the SQLite strftime format string that turns a
// timestamp into a bucket key.
func strftimeFormat(b BucketSize) string {
	switch b {
	case BucketHour:
		return "%Y-%m-%d %H:00:00"
	default:
		return "%Y-%m-%d 00:00:00"
	}
}

// SessionsStarted returns the number of sessions started in each bucket
// over the trailing window. It uses started_at from the sessions table.
func (r *Repo) SessionsStarted(ctx context.Context, bucket BucketSize, window time.Duration) (Series, error) {
	since := time.Now().Add(-window).UTC()
	q := fmt.Sprintf(`SELECT strftime('%s', started_at) AS bucket, COUNT(*) AS n
		FROM sessions WHERE started_at >= ? GROUP BY bucket ORDER BY bucket ASC`, strftimeFormat(bucket))
	return r.queryTimeSeries(ctx, "sessions_started", q, since)
}

// MessagesSent returns the number of messages stored in each bucket. This
// is the closest proxy we have for "Devin requests served" since each
// request to /v1/session/{id}/message inserts one row.
func (r *Repo) MessagesSent(ctx context.Context, bucket BucketSize, window time.Duration) (Series, error) {
	since := time.Now().Add(-window).UTC()
	q := fmt.Sprintf(`SELECT strftime('%s', ts) AS bucket, COUNT(*) AS n
		FROM messages WHERE ts >= ? GROUP BY bucket ORDER BY bucket ASC`, strftimeFormat(bucket))
	return r.queryTimeSeries(ctx, "messages_sent", q, since)
}

// HandoffsCreated returns the number of handoff rows created in each
// bucket — the closest proxy for "how often did a key get exhausted".
func (r *Repo) HandoffsCreated(ctx context.Context, bucket BucketSize, window time.Duration) (Series, error) {
	since := time.Now().Add(-window).UTC()
	q := fmt.Sprintf(`SELECT strftime('%s', created_at) AS bucket, COUNT(*) AS n
		FROM handoffs WHERE created_at >= ? GROUP BY bucket ORDER BY bucket ASC`, strftimeFormat(bucket))
	return r.queryTimeSeries(ctx, "handoffs_created", q, since)
}

// TasksCreated returns the number of new tasks per bucket.
func (r *Repo) TasksCreated(ctx context.Context, bucket BucketSize, window time.Duration) (Series, error) {
	since := time.Now().Add(-window).UTC()
	q := fmt.Sprintf(`SELECT strftime('%s', created_at) AS bucket, COUNT(*) AS n
		FROM tasks WHERE created_at >= ? GROUP BY bucket ORDER BY bucket ASC`, strftimeFormat(bucket))
	return r.queryTimeSeries(ctx, "tasks_created", q, since)
}

func (r *Repo) queryTimeSeries(ctx context.Context, name, query string, args ...any) (Series, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Series{}, fmt.Errorf("observability: %s: %w", name, err)
	}
	defer rows.Close()
	out := Series{Name: name}
	for rows.Next() {
		var bucket string
		var n int
		if err := rows.Scan(&bucket, &n); err != nil {
			return Series{}, fmt.Errorf("observability: scan %s: %w", name, err)
		}
		// SQLite returns the strftime'd value as a string. Parse it back
		// as UTC — the column itself is stored UTC by store.go.
		t, perr := time.Parse("2006-01-02 15:04:05", bucket)
		if perr != nil {
			// Fall back to date-only if we somehow got just YYYY-MM-DD.
			t2, p2 := time.Parse("2006-01-02", bucket)
			if p2 != nil {
				return Series{}, fmt.Errorf("observability: parse bucket %q: %w", bucket, perr)
			}
			t = t2
		}
		out.Points = append(out.Points, TimePoint{BucketStart: t.UTC(), Count: n})
	}
	if err := rows.Err(); err != nil {
		return Series{}, fmt.Errorf("observability: rows %s: %w", name, err)
	}
	return out, nil
}

// SessionStateBreakdown returns a sessions-by-status map for the trailing
// window. Used for a small pie/bar chart on the tab.
func (r *Repo) SessionStateBreakdown(ctx context.Context, window time.Duration) (map[string]int, error) {
	since := time.Now().Add(-window).UTC()
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM sessions
		WHERE started_at >= ? GROUP BY status`, since)
	if err != nil {
		return nil, fmt.Errorf("observability: state breakdown: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// KeyUsageTop returns the most active keys (by request_count) up to limit.
// The web layer joins this with key labels for the leaderboard.
func (r *Repo) KeyUsageTop(ctx context.Context, limit int) ([]KeyUsageRow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, COALESCE(label,''), COALESCE(plan_type,''), state, request_count, COALESCE(last_used_at, '')
		 FROM keys ORDER BY request_count DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("observability: key usage: %w", err)
	}
	defer rows.Close()
	out := []KeyUsageRow{}
	for rows.Next() {
		var row KeyUsageRow
		var lastUsed string
		if err := rows.Scan(&row.ID, &row.Label, &row.Plan, &row.State, &row.Requests, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", lastUsed); err == nil {
				row.LastUsedAt = &t
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// KeyUsageRow is one entry on the "most-used keys" leaderboard.
type KeyUsageRow struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Plan       string     `json:"plan"`
	State      string     `json:"state"`
	Requests   int64      `json:"requests"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// --- Session graph for a single task ---

// SessionNode is a node in the per-task session graph. Each row in the
// sessions table for a task contributes one node; handoffs become edges.
type SessionNode struct {
	ID        string     `json:"id"`
	KeyID     string     `json:"key_id"`
	KeyLabel  string     `json:"key_label"`
	Status    string     `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// SessionEdge connects two sessions: handoff from session A → session B
// (the inheriting session for the same task).
type SessionEdge struct {
	HandoffID string    `json:"handoff_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionGraph is the structure consumed by the front-end SVG renderer.
type SessionGraph struct {
	TaskID string        `json:"task_id"`
	Title  string        `json:"title"`
	Nodes  []SessionNode `json:"nodes"`
	Edges  []SessionEdge `json:"edges"`
}

// SessionGraphForTask returns the session-graph for one task. The shape is
// a directed graph: edges go from from_session_id → to_session_id, where
// a missing to_session_id represents a "handoff was created but the
// replacement session hasn't started yet" state.
func (r *Repo) SessionGraphForTask(ctx context.Context, taskID string) (SessionGraph, error) {
	g := SessionGraph{TaskID: taskID}
	row := r.db.QueryRowContext(ctx, `SELECT title FROM tasks WHERE id = ?`, taskID)
	if err := row.Scan(&g.Title); err != nil {
		if err == sql.ErrNoRows {
			return g, fmt.Errorf("observability: task %s not found", taskID)
		}
		return g, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT s.id, s.key_id, COALESCE(k.label,''), s.status, s.started_at, s.ended_at
		 FROM sessions s LEFT JOIN keys k ON k.id = s.key_id
		 WHERE s.task_id = ? ORDER BY s.started_at ASC`, taskID)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var n SessionNode
		var ended sql.NullTime
		if err := rows.Scan(&n.ID, &n.KeyID, &n.KeyLabel, &n.Status, &n.StartedAt, &ended); err != nil {
			return g, err
		}
		if ended.Valid {
			t := ended.Time
			n.EndedAt = &t
		}
		g.Nodes = append(g.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		return g, err
	}
	hrows, err := r.db.QueryContext(ctx,
		`SELECT id, COALESCE(from_session_id, ''), COALESCE(to_session_id, ''), created_at
		 FROM handoffs WHERE task_id = ? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return g, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var e SessionEdge
		if err := hrows.Scan(&e.HandoffID, &e.From, &e.To, &e.CreatedAt); err != nil {
			return g, err
		}
		g.Edges = append(g.Edges, e)
	}
	return g, hrows.Err()
}
