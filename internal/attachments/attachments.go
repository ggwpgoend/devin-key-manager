// Package attachments implements file uploads from the chat composer.
//
// The user attaches a local file → we upload it to a public file host
// (file.io by default, configurable) → we hand the resulting URL to the
// next Devin prompt so Devin's worker can fetch the bytes. This is the
// C26 "file upload to session" flow from the roadmap.
//
// The provider abstraction means we can swap file.io for transfer.sh, a
// self-hosted nginx, S3 with a signed URL, etc. without touching the
// HTTP/UI layer. file.io was chosen as the default because:
//   - no API key required for small uploads
//   - returns a single-use download link by default (privacy-friendly)
//   - JSON response is trivial to parse
package attachments

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// Status enumerates the lifecycle of a single upload row.
type Status string

const (
	StatusPending  Status = "pending"
	StatusUploaded Status = "uploaded"
	StatusFailed   Status = "failed"
)

// Attachment is one user-uploaded file targeted at a session.
type Attachment struct {
	ID         string
	SessionID  string
	Filename   string
	SizeBytes  int64
	MimeType   string
	Provider   string
	PublicURL  string
	LocalPath  string
	ErrorMsg   string
	Status     Status
	CreatedAt  time.Time
	UploadedAt *time.Time
}

// ErrNotFound is returned when no attachment matches the requested ID.
var ErrNotFound = errors.New("attachments: not found")

// Uploader is the seam between the repo and the actual file host. Tests
// can stub this with a fake to avoid hitting network in unit tests.
type Uploader interface {
	Upload(ctx context.Context, filename, contentType string, body []byte) (string, error)
	Name() string
}

// Repo persists Attachment rows and orchestrates Uploader calls.
type Repo struct {
	db       *store.DB
	uploader Uploader
	now      func() time.Time
}

// NewRepo wires a Repo using the given Uploader. Pass FileIOUploader for
// the default provider; tests should pass a fake.
func NewRepo(db *store.DB, up Uploader) *Repo {
	if up == nil {
		up = &FileIOUploader{Client: http.DefaultClient}
	}
	return &Repo{db: db, uploader: up, now: time.Now}
}

// Create inserts a pending row, uploads the body, and stamps the result.
// Returns the final Attachment so callers can show the public URL right
// away.
func (r *Repo) Create(ctx context.Context, sessionID, filename, mime string, body []byte) (Attachment, error) {
	id := uuid.NewString()
	now := r.now().UTC()
	att := Attachment{
		ID:        id,
		SessionID: sessionID,
		Filename:  filename,
		SizeBytes: int64(len(body)),
		MimeType:  mime,
		Provider:  r.uploader.Name(),
		Status:    StatusPending,
		CreatedAt: now,
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO session_attachments
        (id, session_id, filename, size_bytes, mime_type, provider, public_url, local_path, error_msg, status, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		att.ID, att.SessionID, att.Filename, att.SizeBytes, att.MimeType, att.Provider,
		"", "", "", string(att.Status), att.CreatedAt); err != nil {
		return Attachment{}, fmt.Errorf("attachments: insert: %w", err)
	}

	url, err := r.uploader.Upload(ctx, filename, mime, body)
	if err != nil {
		_, _ = r.db.ExecContext(ctx, `UPDATE session_attachments SET status = ?, error_msg = ? WHERE id = ?`,
			string(StatusFailed), err.Error(), id)
		att.Status = StatusFailed
		att.ErrorMsg = err.Error()
		return att, fmt.Errorf("attachments: upload: %w", err)
	}
	uploadedAt := r.now().UTC()
	if _, err := r.db.ExecContext(ctx, `UPDATE session_attachments
        SET public_url = ?, status = ?, uploaded_at = ? WHERE id = ?`,
		url, string(StatusUploaded), uploadedAt, id); err != nil {
		return att, fmt.Errorf("attachments: persist url: %w", err)
	}
	att.PublicURL = url
	att.Status = StatusUploaded
	att.UploadedAt = &uploadedAt
	return att, nil
}

// ListBySession returns the attachments for a session, newest first.
func (r *Repo) ListBySession(ctx context.Context, sessionID string) ([]Attachment, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, session_id, filename, size_bytes, mime_type,
        provider, public_url, local_path, error_msg, status, created_at, uploaded_at
        FROM session_attachments WHERE session_id = ? ORDER BY created_at DESC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("attachments: list: %w", err)
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var (
			a          Attachment
			status     string
			uploadedAt sql.NullTime
		)
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Filename, &a.SizeBytes, &a.MimeType,
			&a.Provider, &a.PublicURL, &a.LocalPath, &a.ErrorMsg, &status, &a.CreatedAt, &uploadedAt); err != nil {
			return nil, fmt.Errorf("attachments: scan: %w", err)
		}
		a.Status = Status(status)
		if uploadedAt.Valid {
			t := uploadedAt.Time
			a.UploadedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FileIOUploader posts a multipart body to https://file.io/. The endpoint
// is deliberately keyless — we trade one-shot expiry for not needing a
// shared secret in the binary.
type FileIOUploader struct {
	Client   *http.Client
	Endpoint string // override for tests; defaults to "https://file.io/"
}

// Name returns the provider tag for the Attachment row.
func (f *FileIOUploader) Name() string { return "fileio" }

// Upload posts the body to file.io and returns the canonical download URL.
func (f *FileIOUploader) Upload(ctx context.Context, filename, contentType string, body []byte) (string, error) {
	if f.Client == nil {
		f.Client = http.DefaultClient
	}
	endpoint := f.Endpoint
	if endpoint == "" {
		endpoint = "https://file.io/"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("file.io: create part: %w", err)
	}
	if _, err := part.Write(body); err != nil {
		return "", fmt.Errorf("file.io: write part: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("file.io: close mw: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("file.io: new request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := f.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("file.io: do request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("file.io: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		Success bool   `json:"success"`
		Link    string `json:"link"`
		Key     string `json:"key"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("file.io: parse json: %w (body=%s)", err, string(respBody))
	}
	if parsed.Link == "" {
		return "", fmt.Errorf("file.io: empty link in response: %s", parsed.Message)
	}
	return parsed.Link, nil
}
