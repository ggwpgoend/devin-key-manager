// Package notifications stores append-only events surfaced to the browser
// via the Web Notification API. The /events/since endpoint polls this log;
// the frontend pops a desktop toast for each new entry.
//
// The store is intentionally decoupled from sessions/tasks: deleting a task
// does not orphan the event history. RelatedTaskID / RelatedSessionID are
// best-effort backlinks for the click-through URL.
package notifications

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// Kind enumerates the recognised event categories. New events can introduce
// new kinds at any time — consumers should not treat them as exhaustive.
type Kind string

const (
	KindDevinMessage  Kind = "devin_message"
	KindHandoff       Kind = "handoff"
	KindQuota         Kind = "quota"
	KindScheduleFired Kind = "schedule_fired"
	KindSystem        Kind = "system"
)

// Event mirrors a row of the notification_events table.
type Event struct {
	ID               int64
	Kind             Kind
	Title            string
	Body             string
	URL              string
	RelatedTaskID    string
	RelatedSessionID string
	CreatedAt        time.Time
}

// Repo persists Event rows.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo on top of the shared store.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// AppendInput holds the fields for a new event.
type AppendInput struct {
	Kind             Kind
	Title            string
	Body             string
	URL              string
	RelatedTaskID    string
	RelatedSessionID string
}

// Append writes a new event. Returns the assigned ID.
func (r *Repo) Append(ctx context.Context, in AppendInput) (int64, error) {
	if strings.TrimSpace(in.Title) == "" {
		return 0, errors.New("notifications: title required")
	}
	if in.Kind == "" {
		in.Kind = KindSystem
	}
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO notification_events
		    (kind, title, body, url, related_task_id, related_session_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(in.Kind), in.Title, in.Body, in.URL,
		nullIfEmpty(in.RelatedTaskID), nullIfEmpty(in.RelatedSessionID), now,
	)
	if err != nil {
		return 0, fmt.Errorf("notifications: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("notifications: last id: %w", err)
	}
	return id, nil
}

// Since returns up to limit events with id > afterID, ordered ascending so
// the frontend can replay them in the order they happened.
func (r *Repo) Since(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, kind, title, body, url,
		        COALESCE(related_task_id, ''),
		        COALESCE(related_session_id, ''),
		        created_at
		 FROM notification_events
		 WHERE id > ?
		 ORDER BY id ASC
		 LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("notifications: query: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kindStr string
		if err := rows.Scan(&e.ID, &kindStr, &e.Title, &e.Body, &e.URL,
			&e.RelatedTaskID, &e.RelatedSessionID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("notifications: scan: %w", err)
		}
		e.Kind = Kind(kindStr)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Recent returns the last n events newest-first, useful for the timeline page.
func (r *Repo) Recent(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, kind, title, body, url,
		        COALESCE(related_task_id, ''),
		        COALESCE(related_session_id, ''),
		        created_at
		 FROM notification_events
		 ORDER BY id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("notifications: query: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kindStr string
		if err := rows.Scan(&e.ID, &kindStr, &e.Title, &e.Body, &e.URL,
			&e.RelatedTaskID, &e.RelatedSessionID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("notifications: scan: %w", err)
		}
		e.Kind = Kind(kindStr)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return s
}
