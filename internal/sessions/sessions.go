// Package sessions models the lifecycle of a single Devin session and the
// cached message stream for each session.
//
// One task may produce multiple Session rows over its lifetime — every time
// the active key hits a quota the manager closes the running session, picks a
// new key, and opens a fresh Session referencing the same task. The Devin
// session ID (issued by the Devin Cloud API) is stored on each Session so the
// background poller can fetch updates.
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// Status enumerates the lifecycle states of a single Devin session as tracked
// by this manager. The CHECK constraint in migrations/0001_init.sql is the
// canonical source of valid values.
type Status string

const (
	StatusCreating       Status = "creating"
	StatusRunning        Status = "running"
	StatusBlocked        Status = "blocked"
	StatusCompleted      Status = "completed"
	StatusFailed         Status = "failed"
	StatusQuotaExhausted Status = "quota_exhausted"
	StatusHandoffPending Status = "handoff_pending"
	StatusHandoffDone    Status = "handoff_done"
)

// Valid returns true if s is a recognised session status.
func (s Status) Valid() bool {
	switch s {
	case StatusCreating, StatusRunning, StatusBlocked, StatusCompleted,
		StatusFailed, StatusQuotaExhausted, StatusHandoffPending, StatusHandoffDone:
		return true
	default:
		return false
	}
}

// Role enumerates the participant types in a session conversation. The
// constraint in migrations/0001_init.sql allows {user, assistant, system}.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Session is the in-memory representation of a row from the sessions table.
type Session struct {
	ID             string
	TaskID         string
	KeyID          string
	DevinSessionID string
	Status         Status
	StartedAt      time.Time
	EndedAt        *time.Time
	EndReason      string
	LastPolledAt   *time.Time
	Notes          string
}

// Message is a single entry in the session conversation cache.
type Message struct {
	ID        string
	SessionID string
	Role      Role
	Content   string
	Timestamp time.Time
}

// ErrNotFound is returned when no session matches the given ID.
var ErrNotFound = errors.New("sessions: not found")

// Repo manages Session and Message persistence.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo on top of the shared store.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// CreateInput holds the fields required to register a new session.
type CreateInput struct {
	TaskID string
	KeyID  string
}

// Create inserts a new session in the "creating" state. The Devin session ID
// is filled in later by AttachDevinSessionID once the API has issued one.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Session, error) {
	if strings.TrimSpace(in.TaskID) == "" {
		return Session{}, errors.New("sessions: task id required")
	}
	if strings.TrimSpace(in.KeyID) == "" {
		return Session{}, errors.New("sessions: key id required")
	}
	id := uuid.NewString()
	now := r.now().UTC()
	_, err := r.db.ExecContext(ctx, `INSERT INTO sessions
        (id, task_id, key_id, status, started_at)
        VALUES (?, ?, ?, 'creating', ?)`,
		id, in.TaskID, in.KeyID, now)
	if err != nil {
		return Session{}, fmt.Errorf("sessions: insert: %w", err)
	}
	return Session{
		ID:        id,
		TaskID:    in.TaskID,
		KeyID:     in.KeyID,
		Status:    StatusCreating,
		StartedAt: now,
	}, nil
}

// AttachDevinSessionID records the Devin Cloud session identifier returned
// from POST /v1/sessions and transitions status to running.
func (r *Repo) AttachDevinSessionID(ctx context.Context, id, devinSessionID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET devin_session_id = ?, status = 'running' WHERE id = ?`,
		devinSessionID, id)
	if err != nil {
		return fmt.Errorf("sessions: attach: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus transitions a session into the given state, optionally recording
// an end reason for terminal statuses.
func (r *Repo) SetStatus(ctx context.Context, id string, status Status, endReason string) error {
	if !status.Valid() {
		return fmt.Errorf("sessions: invalid status %q", status)
	}
	now := r.now().UTC()
	terminal := status == StatusCompleted || status == StatusFailed ||
		status == StatusQuotaExhausted || status == StatusHandoffDone
	var (
		res sql.Result
		err error
	)
	if terminal {
		res, err = r.db.ExecContext(ctx,
			`UPDATE sessions SET status = ?, ended_at = ?, end_reason = ? WHERE id = ?`,
			string(status), now, endReason, id)
	} else {
		res, err = r.db.ExecContext(ctx,
			`UPDATE sessions SET status = ? WHERE id = ?`,
			string(status), id)
	}
	if err != nil {
		return fmt.Errorf("sessions: set status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkPolled stamps last_polled_at = now. Used by the background poller so the
// UI can show "last refreshed at …".
func (r *Repo) MarkPolled(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET last_polled_at = ? WHERE id = ?`,
		r.now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sessions: mark polled: %w", err)
	}
	return nil
}

// Get loads a single session by local ID.
func (r *Repo) Get(ctx context.Context, id string) (Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, task_id, key_id, COALESCE(devin_session_id,''), status,
		        started_at, ended_at, COALESCE(end_reason,''), last_polled_at,
		        COALESCE(notes,'')
		 FROM sessions WHERE id = ?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// SetNotes persists the user's private notes for a session. The notes are
// never forwarded to Devin; they live only in the local SQLite store.
func (r *Repo) SetNotes(ctx context.Context, id, notes string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET notes = ? WHERE id = ?`, notes, id)
	if err != nil {
		return fmt.Errorf("sessions: set notes: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByTask returns all sessions for a task, newest first.
func (r *Repo) ListByTask(ctx context.Context, taskID string) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, task_id, key_id, COALESCE(devin_session_id,''), status,
		        started_at, ended_at, COALESCE(end_reason,''), last_polled_at,
		        COALESCE(notes,'')
		 FROM sessions WHERE task_id = ? ORDER BY started_at DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("sessions: list by task: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListActive returns sessions that should still be polled (status creating,
// running, blocked, or handoff_pending).
func (r *Repo) ListActive(ctx context.Context) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, task_id, key_id, COALESCE(devin_session_id,''), status,
		        started_at, ended_at, COALESCE(end_reason,''), last_polled_at,
		        COALESCE(notes,'')
		 FROM sessions
		 WHERE status IN ('creating','running','blocked','handoff_pending')
		 ORDER BY started_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("sessions: list active: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionStats summarises the session table for dashboard tiles.
type SessionStats struct {
	Total       int
	Open        int
	Closed      int
	StartedLast time.Time
	Last24h     int
}

// Stats returns aggregated session counts. Open = creating/running/blocked/handoff_pending.
// Last24h = sessions where started_at within the last 24h.
func (r *Repo) Stats(ctx context.Context) (SessionStats, error) {
	var st SessionStats
	row := r.db.QueryRowContext(ctx, `SELECT
        COUNT(*) AS total,
        SUM(CASE WHEN status IN ('creating','running','blocked','handoff_pending') THEN 1 ELSE 0 END) AS open,
        SUM(CASE WHEN status NOT IN ('creating','running','blocked','handoff_pending') THEN 1 ELSE 0 END) AS closed,
        SUM(CASE WHEN started_at >= ? THEN 1 ELSE 0 END) AS last24h
    FROM sessions`, r.now().Add(-24*time.Hour).UTC())
	var open, closed, last24h sql.NullInt64
	if err := row.Scan(&st.Total, &open, &closed, &last24h); err != nil {
		return st, fmt.Errorf("sessions: stats: %w", err)
	}
	st.Open = int(open.Int64)
	st.Closed = int(closed.Int64)
	st.Last24h = int(last24h.Int64)
	return st, nil
}

// AppendMessage inserts a message into the cache for a session. The caller
// supplies the timestamp so messages reflected from Devin preserve their
// original ordering even after a re-sync.
func (r *Repo) AppendMessage(ctx context.Context, sessionID string, role Role, content string, ts time.Time) (Message, error) {
	id := uuid.NewString()
	if ts.IsZero() {
		ts = r.now().UTC()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, ts) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, string(role), content, ts.UTC())
	if err != nil {
		return Message{}, fmt.Errorf("sessions: append message: %w", err)
	}
	return Message{
		ID:        id,
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		Timestamp: ts.UTC(),
	}, nil
}

// ListMessages returns the conversation cache for a session ordered by
// timestamp ascending (oldest first), which is how the chat view wants it.
func (r *Repo) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, ts FROM messages
		 WHERE session_id = ? ORDER BY ts ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessions: list messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m    Message
			role string
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &role, &m.Content, &m.Timestamp); err != nil {
			return nil, fmt.Errorf("sessions: scan message: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceMessages atomically swaps out the cached message stream for a
// session. Used by the poller when re-syncing from Devin: rather than trying
// to diff against existing rows (which is error-prone given that Devin may
// edit historical messages), we just truncate and re-insert.
func (r *Repo) ReplaceMessages(ctx context.Context, sessionID string, msgs []Message) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("sessions: truncate: %w", err)
	}
	for _, m := range msgs {
		id := m.ID
		if id == "" {
			id = uuid.NewString()
		}
		ts := m.Timestamp
		if ts.IsZero() {
			ts = r.now().UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, content, ts) VALUES (?, ?, ?, ?, ?)`,
			id, sessionID, string(m.Role), m.Content, ts.UTC()); err != nil {
			return fmt.Errorf("sessions: insert message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions: commit: %w", err)
	}
	return nil
}

// CountMessages returns the cached message count for a session.
func (r *Repo) CountMessages(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sessions: count messages: %w", err)
	}
	return n, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(s rowScanner) (Session, error) {
	var (
		sess         Session
		status       string
		endedAt      sql.NullTime
		lastPolledAt sql.NullTime
	)
	if err := s.Scan(&sess.ID, &sess.TaskID, &sess.KeyID, &sess.DevinSessionID,
		&status, &sess.StartedAt, &endedAt, &sess.EndReason, &lastPolledAt, &sess.Notes); err != nil {
		return Session{}, fmt.Errorf("sessions: scan: %w", err)
	}
	sess.Status = Status(status)
	if endedAt.Valid {
		t := endedAt.Time
		sess.EndedAt = &t
	}
	if lastPolledAt.Valid {
		t := lastPolledAt.Time
		sess.LastPolledAt = &t
	}
	return sess, nil
}
