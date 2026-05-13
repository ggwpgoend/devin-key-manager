// Package tasks owns the lifecycle of a user task. A task is the logical
// goal the user submits ("write me a Windows GUI for FFmpeg") and may span
// multiple Devin sessions across multiple API keys as quotas exhaust.
package tasks

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

// Status enumerates the high-level lifecycle states of a task. The string
// values match the CHECK constraint in migrations/0001_init.sql.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Valid returns true if s is a recognised task status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusPaused, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// Task is the in-memory representation of a row from the tasks table.
type Task struct {
	ID            string
	Title         string
	InitialPrompt string
	Status        Status
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ErrNotFound is returned when a task with the given ID does not exist.
var ErrNotFound = errors.New("tasks: not found")

// Repo persists Task rows.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo on top of the shared store. The now function is
// overridable for tests; nil means time.Now.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// CreateInput captures the user-supplied fields needed to start a task.
type CreateInput struct {
	Title  string
	Prompt string
}

// Create inserts a new pending task.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Task, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Task{}, errors.New("tasks: title is required")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return Task{}, errors.New("tasks: prompt is required")
	}
	id := uuid.NewString()
	now := r.now().UTC()
	_, err := r.db.ExecContext(ctx, `INSERT INTO tasks
        (id, title, initial_prompt, status, created_at, updated_at)
        VALUES (?, ?, ?, 'pending', ?, ?)`,
		id, title, prompt, now, now)
	if err != nil {
		return Task{}, fmt.Errorf("tasks: insert: %w", err)
	}
	return Task{
		ID:            id,
		Title:         title,
		InitialPrompt: prompt,
		Status:        StatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// List returns all tasks ordered by most recent activity first.
func (r *Repo) List(ctx context.Context) ([]Task, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, title, initial_prompt, status, created_at, updated_at
		 FROM tasks
		 ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("tasks: list: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Get loads a single task by ID.
func (r *Repo) Get(ctx context.Context, id string) (Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, title, initial_prompt, status, created_at, updated_at
		 FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

// SetStatus transitions the task to the given status. No transition graph is
// enforced at the storage layer — the orchestrator is responsible for valid
// moves. We update updated_at so the UI sorts active work to the top.
func (r *Repo) SetStatus(ctx context.Context, id string, status Status) error {
	if !status.Valid() {
		return fmt.Errorf("tasks: invalid status %q", status)
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), r.now().UTC(), id)
	if err != nil {
		return fmt.Errorf("tasks: set status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Touch bumps updated_at to now without changing status. Used when a session
// produces new messages so the task sorts to the top of the list.
func (r *Repo) Touch(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET updated_at = ? WHERE id = ?`,
		r.now().UTC(), id)
	if err != nil {
		return fmt.Errorf("tasks: touch: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(s rowScanner) (Task, error) {
	var (
		t      Task
		status string
	)
	if err := s.Scan(&t.ID, &t.Title, &t.InitialPrompt, &status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Task{}, fmt.Errorf("tasks: scan: %w", err)
	}
	t.Status = Status(status)
	return t, nil
}
