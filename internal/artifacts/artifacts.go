// Package artifacts stores files emitted by Devin during a session — typically
// screenshots, generated binaries, and any other URL that Devin posts in the
// chat. The poller (manager.SyncSession) extracts URLs from incoming messages
// and asks this package to materialise them on the local disk so the UI can
// render images inline and offer download links for everything else.
//
// One row per (session_id, remote_url). The file itself lives under
// <ArtifactsDir>/<session_id>/<artifact_id><ext>, where ArtifactsDir defaults
// to ./artifacts/ but is configurable via the manager's config.
package artifacts

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

// Source identifies who produced an artifact.
type Source string

const (
	SourceDevin Source = "devin"
	SourceUser  Source = "user"
)

// Status enumerates the download lifecycle.
type Status string

const (
	StatusPending     Status = "pending"
	StatusDownloading Status = "downloading"
	StatusReady       Status = "ready"
	StatusFailed      Status = "failed"
)

// Artifact is the in-memory representation of a row in the artifacts table.
type Artifact struct {
	ID          string
	TaskID      string
	SessionID   string
	Filename    string
	LocalPath   string
	RemoteURL   string
	SHA256      string
	SizeBytes   int64
	ContentType string
	Source      Source
	Status      Status
	Error       string
	CreatedAt   time.Time
}

// IsImage reports whether the artifact's content type suggests it can be
// rendered inline via an <img> tag. We branch on the broad image/* family;
// the browser will deal with sniffing the format underneath.
func (a Artifact) IsImage() bool {
	return strings.HasPrefix(strings.ToLower(a.ContentType), "image/")
}

// ErrNotFound is returned by Repo lookups when no matching row exists.
var ErrNotFound = errors.New("artifacts: not found")

// ErrAlreadyExists is returned by Create when (session_id, remote_url) is
// already present. Callers treat this as a soft success — the original row is
// already taking care of downloading the same file.
var ErrAlreadyExists = errors.New("artifacts: already exists for this session+url")

// Repo persists artifact metadata.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo on top of the shared database.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// CreateInput captures the fields needed to register a new artifact.
type CreateInput struct {
	TaskID    string
	SessionID string
	Filename  string
	RemoteURL string
	Source    Source
}

// Create inserts a pending artifact row. If (SessionID, RemoteURL) is already
// present, returns ErrAlreadyExists with the existing artifact populated.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Artifact, error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return Artifact{}, errors.New("artifacts: session id required")
	}
	if strings.TrimSpace(in.RemoteURL) == "" {
		return Artifact{}, errors.New("artifacts: remote url required")
	}
	if in.Source == "" {
		in.Source = SourceDevin
	}
	if existing, err := r.GetByURL(ctx, in.SessionID, in.RemoteURL); err == nil {
		return existing, ErrAlreadyExists
	}
	id := uuid.NewString()
	filename := strings.TrimSpace(in.Filename)
	if filename == "" {
		filename = id
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO artifacts (id, task_id, session_id, filename, local_path,
		   devin_url, content_type, source, status, error)
		 VALUES (?, ?, ?, ?, '', ?, '', ?, ?, '')`,
		id, in.TaskID, in.SessionID, filename, in.RemoteURL, string(in.Source), string(StatusPending),
	); err != nil {
		return Artifact{}, fmt.Errorf("artifacts: insert: %w", err)
	}
	return Artifact{
		ID:        id,
		TaskID:    in.TaskID,
		SessionID: in.SessionID,
		Filename:  filename,
		RemoteURL: in.RemoteURL,
		Source:    in.Source,
		Status:    StatusPending,
		CreatedAt: r.now().UTC(),
	}, nil
}

// MarkDownloading flips the row status to "downloading". The downloader uses
// this to claim ownership before fetching the body.
func (r *Repo) MarkDownloading(ctx context.Context, id string) error {
	return r.setStatus(ctx, id, StatusDownloading, "")
}

// MarkReady stores the on-disk path, content metadata, and flips status to
// "ready" so the UI can render the artifact.
type ReadyInput struct {
	LocalPath   string
	ContentType string
	SizeBytes   int64
	SHA256      string
	Filename    string
}

// MarkReady persists the on-disk path + metadata for a successfully
// downloaded artifact and transitions status to ready.
func (r *Repo) MarkReady(ctx context.Context, id string, in ReadyInput) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE artifacts
		   SET local_path = ?, content_type = ?, size_bytes = ?, sha256 = ?,
		       filename = COALESCE(NULLIF(?, ''), filename),
		       status = ?, error = ''
		 WHERE id = ?`,
		in.LocalPath, in.ContentType, in.SizeBytes, in.SHA256, in.Filename, string(StatusReady), id,
	); err != nil {
		return fmt.Errorf("artifacts: mark ready: %w", err)
	}
	return nil
}

// MarkFailed records a failed download so the UI can surface the error and
// the downloader can decide whether to retry.
func (r *Repo) MarkFailed(ctx context.Context, id, errMsg string) error {
	return r.setStatus(ctx, id, StatusFailed, errMsg)
}

func (r *Repo) setStatus(ctx context.Context, id string, status Status, errMsg string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE artifacts SET status = ?, error = ? WHERE id = ?`,
		string(status), errMsg, id)
	if err != nil {
		return fmt.Errorf("artifacts: set status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get loads a single artifact by ID.
func (r *Repo) Get(ctx context.Context, id string) (Artifact, error) {
	row := r.db.QueryRowContext(ctx, baseSelect+" WHERE id = ?", id)
	a, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return a, err
}

// GetByURL looks up an artifact by (session, url). Returns ErrNotFound when
// nothing matches.
func (r *Repo) GetByURL(ctx context.Context, sessionID, url string) (Artifact, error) {
	row := r.db.QueryRowContext(ctx, baseSelect+" WHERE session_id = ? AND devin_url = ?",
		sessionID, url)
	a, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return a, err
}

// ListBySession returns artifacts for one session ordered newest-first.
func (r *Repo) ListBySession(ctx context.Context, sessionID string) ([]Artifact, error) {
	rows, err := r.db.QueryContext(ctx, baseSelect+
		" WHERE session_id = ? ORDER BY created_at DESC, id DESC", sessionID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list session: %w", err)
	}
	defer rows.Close()
	return scanAll(rows)
}

// ListByTask returns artifacts across all sessions of a task.
func (r *Repo) ListByTask(ctx context.Context, taskID string) ([]Artifact, error) {
	rows, err := r.db.QueryContext(ctx, baseSelect+
		" WHERE task_id = ? ORDER BY created_at DESC, id DESC", taskID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list task: %w", err)
	}
	defer rows.Close()
	return scanAll(rows)
}

// ListPending returns artifacts that still need to be downloaded. Used by the
// downloader worker on startup and after restarts to resume interrupted jobs.
func (r *Repo) ListPending(ctx context.Context) ([]Artifact, error) {
	rows, err := r.db.QueryContext(ctx, baseSelect+
		" WHERE status IN (?, ?) ORDER BY created_at ASC",
		string(StatusPending), string(StatusDownloading))
	if err != nil {
		return nil, fmt.Errorf("artifacts: list pending: %w", err)
	}
	defer rows.Close()
	return scanAll(rows)
}

const baseSelect = `SELECT id, COALESCE(task_id, ''), COALESCE(session_id, ''),
                          filename, COALESCE(local_path, ''),
                          COALESCE(devin_url, ''), COALESCE(sha256, ''),
                          COALESCE(size_bytes, 0), content_type,
                          source, status, error, created_at
                   FROM artifacts`

type scanner interface {
	Scan(dest ...any) error
}

func scan(s scanner) (Artifact, error) {
	var (
		a              Artifact
		source, status string
	)
	if err := s.Scan(&a.ID, &a.TaskID, &a.SessionID, &a.Filename, &a.LocalPath,
		&a.RemoteURL, &a.SHA256, &a.SizeBytes, &a.ContentType,
		&source, &status, &a.Error, &a.CreatedAt); err != nil {
		return Artifact{}, err
	}
	a.Source = Source(source)
	a.Status = Status(status)
	return a, nil
}

func scanAll(rows *sql.Rows) ([]Artifact, error) {
	var out []Artifact
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
