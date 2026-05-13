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
	// PR-12 B14: fork bookkeeping. When this row is itself a fork off
	// another session, ForkedFromSessionID points back at the parent and
	// ForkedFromMessageID records the anchor message we forked AT (the
	// last message that survived into the new branch).
	ForkedFromSessionID string
	ForkedFromMessageID string
	ForkedAt            *time.Time
}

// Message is a single entry in the session conversation cache.
type Message struct {
	ID        string
	SessionID string
	Role      Role
	Content   string
	Timestamp time.Time
	// PR-12: a user-flagged "this is the important part" marker. Pinned
	// messages float to the top of the session view and are excluded from
	// the FTS-search noise filter.
	Pinned   bool
	PinnedAt *time.Time
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
		        COALESCE(notes,''), forked_from_session_id, forked_from_message_id, forked_at
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
		        COALESCE(notes,''), forked_from_session_id, forked_from_message_id, forked_at
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
		        COALESCE(notes,''), forked_from_session_id, forked_from_message_id, forked_at
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
		`SELECT id, session_id, role, content, ts, pinned, pinned_at FROM messages
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
		var pinnedAt sql.NullTime
		var pinned int
		if err := rows.Scan(&m.ID, &m.SessionID, &role, &m.Content, &m.Timestamp, &pinned, &pinnedAt); err != nil {
			return nil, fmt.Errorf("sessions: scan message: %w", err)
		}
		m.Role = Role(role)
		m.Pinned = pinned != 0
		if pinnedAt.Valid {
			t := pinnedAt.Time
			m.PinnedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetPinned toggles the pinned flag on a single message. When pinning we
// stamp pinned_at = now() so the UI can sort pinned messages by recency of
// the pin action (not by message timestamp).
func (r *Repo) SetPinned(ctx context.Context, messageID string, pinned bool) error {
	now := r.now().UTC()
	var res sql.Result
	var err error
	if pinned {
		res, err = r.db.ExecContext(ctx, `UPDATE messages SET pinned = 1, pinned_at = ? WHERE id = ?`, now, messageID)
	} else {
		res, err = r.db.ExecContext(ctx, `UPDATE messages SET pinned = 0, pinned_at = NULL WHERE id = ?`, messageID)
	}
	if err != nil {
		return fmt.Errorf("sessions: set pinned: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForkInput parameterises a B14 checkpoint fork: from which session,
// at which anchor message (inclusive), into which key and task. KeyID
// defaults to the source session's key when zero; TaskID defaults to
// the source session's task when zero.
type ForkInput struct {
	SourceID        string
	AnchorMessageID string
	TaskID          string
	KeyID           string
}

// Fork creates a new local session whose message history is a verbatim
// copy of the source session up to (and including) the anchor message.
// The returned session has status="creating" and an empty DevinSessionID
// — the caller is expected to open a fresh Devin session and AttachDevinSessionID
// before sending the first follow-up. The fork lineage (forked_from_*)
// is persisted so the UI can show breadcrumbs.
//
// We deliberately copy messages locally and do NOT replay them against
// Devin: Devin would re-process every line and rack up cost. The forked
// session starts "cold" on Devin's side and the user can resume by sending
// any new message.
func (r *Repo) Fork(ctx context.Context, in ForkInput) (Session, error) {
	src, err := r.Get(ctx, in.SourceID)
	if err != nil {
		return Session{}, err
	}
	taskID := in.TaskID
	if taskID == "" {
		taskID = src.TaskID
	}
	keyID := in.KeyID
	if keyID == "" {
		keyID = src.KeyID
	}

	// Resolve the anchor: walk the source's message list and copy
	// everything with ts <= anchor.Timestamp. If anchor not given,
	// fork at the END (full copy).
	allMsgs, err := r.ListMessages(ctx, in.SourceID)
	if err != nil {
		return Session{}, err
	}
	cutoff := -1
	if strings.TrimSpace(in.AnchorMessageID) != "" {
		for i, m := range allMsgs {
			if m.ID == in.AnchorMessageID {
				cutoff = i
				break
			}
		}
		if cutoff < 0 {
			return Session{}, fmt.Errorf("sessions: fork: anchor message %s not in session %s", in.AnchorMessageID, in.SourceID)
		}
	} else if len(allMsgs) > 0 {
		cutoff = len(allMsgs) - 1
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("sessions: fork begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	newID := uuid.NewString()
	now := r.now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions
        (id, task_id, key_id, status, started_at, notes, forked_from_session_id, forked_from_message_id, forked_at)
        VALUES (?, ?, ?, 'creating', ?, ?, ?, ?, ?)`,
		newID, taskID, keyID, now, src.Notes, src.ID, nullStringOrNil(in.AnchorMessageID), now)
	if err != nil {
		return Session{}, fmt.Errorf("sessions: fork insert: %w", err)
	}
	for i := 0; i <= cutoff; i++ {
		m := allMsgs[i]
		ts := m.Timestamp
		if ts.IsZero() {
			ts = now
		}
		pinned := 0
		var pinnedAt any
		if m.Pinned {
			pinned = 1
			if m.PinnedAt != nil {
				pinnedAt = *m.PinnedAt
			} else {
				pinnedAt = now
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, content, ts, pinned, pinned_at)
             VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), newID, string(m.Role), m.Content, ts.UTC(), pinned, pinnedAt); err != nil {
			return Session{}, fmt.Errorf("sessions: fork copy message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("sessions: fork commit: %w", err)
	}
	return r.Get(ctx, newID)
}

func nullStringOrNil(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// PinnedMessages returns all messages in a session that are currently
// pinned, ordered by pin time descending. Cheap because pinned is a tiny
// subset of total messages.
func (r *Repo) PinnedMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, ts, pinned, pinned_at FROM messages
		 WHERE session_id = ? AND pinned = 1
		 ORDER BY pinned_at DESC, ts DESC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessions: list pinned: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m        Message
			role     string
			pinned   int
			pinnedAt sql.NullTime
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &role, &m.Content, &m.Timestamp, &pinned, &pinnedAt); err != nil {
			return nil, fmt.Errorf("sessions: scan pinned: %w", err)
		}
		m.Role = Role(role)
		m.Pinned = pinned != 0
		if pinnedAt.Valid {
			t := pinnedAt.Time
			m.PinnedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchHit pairs a message with the session/task it belongs to so the UI
// can render a single hit row without extra lookups.
type SearchHit struct {
	Message   Message
	TaskID    string
	TaskTitle string
	Snippet   string
}

// SearchMessages runs an FTS5 query over the messages.content index and
// returns hits scoped to a single session (when sessionID != "") or across
// all sessions (when sessionID == ""). The snippet column is a 6-word
// excerpt around the match suitable for the result list. Limit caps the
// result count; 50 is a reasonable default for the chat search bar.
func (r *Repo) SearchMessages(ctx context.Context, query, sessionID string, limit int) ([]SearchHit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	// FTS5 syntax allows boolean operators, but we want forgiving user
	// input — quote the whole thing as a phrase and let FTS5 tokenise.
	phrase := "\"" + strings.ReplaceAll(q, "\"", "\"\"") + "\""
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID == "" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT m.id, m.session_id, m.role, m.content, m.ts, m.pinned, m.pinned_at,
			       s.task_id, COALESCE(t.title, ''),
			       snippet(messages_fts, 0, '<mark>', '</mark>', '…', 12)
			FROM messages_fts
			JOIN messages m ON m.rowid = messages_fts.rowid
			JOIN sessions s ON s.id = m.session_id
			LEFT JOIN tasks t ON t.id = s.task_id
			WHERE messages_fts MATCH ?
			ORDER BY rank LIMIT ?`, phrase, limit)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT m.id, m.session_id, m.role, m.content, m.ts, m.pinned, m.pinned_at,
			       s.task_id, COALESCE(t.title, ''),
			       snippet(messages_fts, 0, '<mark>', '</mark>', '…', 12)
			FROM messages_fts
			JOIN messages m ON m.rowid = messages_fts.rowid
			JOIN sessions s ON s.id = m.session_id
			LEFT JOIN tasks t ON t.id = s.task_id
			WHERE messages_fts MATCH ? AND m.session_id = ?
			ORDER BY rank LIMIT ?`, phrase, sessionID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("sessions: fts search: %w", err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var (
			h        SearchHit
			role     string
			pinned   int
			pinnedAt sql.NullTime
		)
		if err := rows.Scan(&h.Message.ID, &h.Message.SessionID, &role, &h.Message.Content, &h.Message.Timestamp, &pinned, &pinnedAt,
			&h.TaskID, &h.TaskTitle, &h.Snippet); err != nil {
			return nil, fmt.Errorf("sessions: scan search: %w", err)
		}
		h.Message.Role = Role(role)
		h.Message.Pinned = pinned != 0
		if pinnedAt.Valid {
			t := pinnedAt.Time
			h.Message.PinnedAt = &t
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReplaceMessages atomically swaps out the cached message stream for a
// session. Used by the poller when re-syncing from Devin: rather than trying
// to diff against existing rows (which is error-prone given that Devin may
// edit historical messages), we just truncate and re-insert. We preserve
// the pinned flag (PR-12) keyed by content-hash so a re-sync doesn't lose
// user-set pins.
func (r *Repo) ReplaceMessages(ctx context.Context, sessionID string, msgs []Message) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Snapshot existing pins so we can re-apply them after the swap.
	pinRows, err := tx.QueryContext(ctx, `SELECT content, pinned_at FROM messages WHERE session_id = ? AND pinned = 1`, sessionID)
	if err != nil {
		return fmt.Errorf("sessions: snapshot pins: %w", err)
	}
	pins := map[string]time.Time{}
	for pinRows.Next() {
		var c string
		var pa sql.NullTime
		if err := pinRows.Scan(&c, &pa); err != nil {
			pinRows.Close()
			return fmt.Errorf("sessions: scan pin: %w", err)
		}
		if pa.Valid {
			pins[c] = pa.Time
		} else {
			pins[c] = r.now().UTC()
		}
	}
	pinRows.Close()

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
		pinned := 0
		var pinnedAt any
		if pa, ok := pins[m.Content]; ok {
			pinned = 1
			pinnedAt = pa
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, content, ts, pinned, pinned_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, sessionID, string(m.Role), m.Content, ts.UTC(), pinned, pinnedAt); err != nil {
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
		forkedFrom   sql.NullString
		forkedMsg    sql.NullString
		forkedAt     sql.NullTime
	)
	if err := s.Scan(&sess.ID, &sess.TaskID, &sess.KeyID, &sess.DevinSessionID,
		&status, &sess.StartedAt, &endedAt, &sess.EndReason, &lastPolledAt, &sess.Notes,
		&forkedFrom, &forkedMsg, &forkedAt); err != nil {
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
	if forkedFrom.Valid {
		sess.ForkedFromSessionID = forkedFrom.String
	}
	if forkedMsg.Valid {
		sess.ForkedFromMessageID = forkedMsg.String
	}
	if forkedAt.Valid {
		t := forkedAt.Time
		sess.ForkedAt = &t
	}
	return sess, nil
}
