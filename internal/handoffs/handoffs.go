// Package handoffs records the chain of sessions that make up a single
// logical task. When the active key for a task runs out of quota, the manager
// inserts a handoff row linking the dying session to the freshly-started one
// running on a different key, with a markdown summary of the conversation so
// far so Devin can pick up where it left off.
package handoffs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// ErrNotFound is returned when a handoff lookup misses.
var ErrNotFound = errors.New("handoffs: not found")

// Handoff is the in-memory representation of a row from the handoffs table.
// ToSessionID may be empty when the row has been created but the replacement
// session has not been opened yet.
type Handoff struct {
	ID            string
	TaskID        string
	FromSessionID string
	ToSessionID   string
	Markdown      string
	Attachments   []string
	CreatedAt     time.Time
}

// Repo manages handoff rows.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo around the shared store.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// CreateInput holds the fields required to record a fresh handoff.
type CreateInput struct {
	TaskID        string
	FromSessionID string
	Markdown      string
	Attachments   []string
}

// Create inserts a handoff row. The replacement session id is filled in later
// via LinkTo once the new session has been opened.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Handoff, error) {
	if strings.TrimSpace(in.TaskID) == "" {
		return Handoff{}, errors.New("handoffs: task id required")
	}
	if strings.TrimSpace(in.Markdown) == "" {
		return Handoff{}, errors.New("handoffs: markdown body required")
	}
	id := uuid.NewString()
	now := r.now().UTC()
	attachments := in.Attachments
	if attachments == nil {
		attachments = []string{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return Handoff{}, fmt.Errorf("handoffs: marshal attachments: %w", err)
	}

	var fromID sql.NullString
	if in.FromSessionID != "" {
		fromID = sql.NullString{String: in.FromSessionID, Valid: true}
	}

	_, err = r.db.ExecContext(ctx, `INSERT INTO handoffs
        (id, task_id, from_session_id, to_session_id, markdown, attachments_json, created_at)
        VALUES (?, ?, ?, NULL, ?, ?, ?)`,
		id, in.TaskID, fromID, in.Markdown, string(attachmentsJSON), now)
	if err != nil {
		return Handoff{}, fmt.Errorf("handoffs: insert: %w", err)
	}
	return Handoff{
		ID:            id,
		TaskID:        in.TaskID,
		FromSessionID: in.FromSessionID,
		Markdown:      in.Markdown,
		Attachments:   attachments,
		CreatedAt:     now,
	}, nil
}

// LinkTo stamps the new session id on an existing handoff row, completing the
// chain from the dying session to its replacement.
func (r *Repo) LinkTo(ctx context.Context, handoffID, toSessionID string) error {
	if strings.TrimSpace(toSessionID) == "" {
		return errors.New("handoffs: to session id required")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE handoffs SET to_session_id = ? WHERE id = ?`,
		toSessionID, handoffID)
	if err != nil {
		return fmt.Errorf("handoffs: link to: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByTask returns every handoff for a task, oldest first.
func (r *Repo) ListByTask(ctx context.Context, taskID string) ([]Handoff, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT
        id, task_id, COALESCE(from_session_id, ''), COALESCE(to_session_id, ''),
        markdown, attachments_json, created_at
        FROM handoffs WHERE task_id = ? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("handoffs: list by task: %w", err)
	}
	defer rows.Close()
	var out []Handoff
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetForSession returns the handoff that begat the given session (i.e. the
// row whose to_session_id matches), or ErrNotFound when this session was
// opened directly by the user rather than as a rotation target.
func (r *Repo) GetForSession(ctx context.Context, sessionID string) (Handoff, error) {
	row := r.db.QueryRowContext(ctx, `SELECT
        id, task_id, COALESCE(from_session_id, ''), COALESCE(to_session_id, ''),
        markdown, attachments_json, created_at
        FROM handoffs WHERE to_session_id = ?`, sessionID)
	h, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, ErrNotFound
	}
	return h, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanHandoff(s rowScanner) (Handoff, error) {
	var (
		h               Handoff
		attachmentsJSON string
	)
	if err := s.Scan(&h.ID, &h.TaskID, &h.FromSessionID, &h.ToSessionID,
		&h.Markdown, &attachmentsJSON, &h.CreatedAt); err != nil {
		return Handoff{}, fmt.Errorf("handoffs: scan: %w", err)
	}
	if attachmentsJSON != "" {
		if err := json.Unmarshal([]byte(attachmentsJSON), &h.Attachments); err != nil {
			return Handoff{}, fmt.Errorf("handoffs: unmarshal attachments: %w", err)
		}
	}
	return h, nil
}
