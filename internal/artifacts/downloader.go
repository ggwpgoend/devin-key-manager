package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxArtifactBytes is the hard cap on how large a single artifact may grow.
// Anything bigger is rejected to keep the local artifacts directory from
// filling up disk because a Devin session uploaded a multi-GB log.
const MaxArtifactBytes int64 = 256 << 20 // 256 MiB

// BearerProvider yields the Bearer token to use when fetching a session's
// attachments. The manager wires this through Reveal() so the downloader can
// authenticate without ever storing plaintext keys on its struct.
type BearerProvider func(ctx context.Context, sessionID string) (string, error)

// Downloader streams remote attachment URLs to <root>/<session_id>/<artifact_id><ext>
// and flips the artifact row to ready/failed in the Repo as appropriate. One
// Downloader serves the whole process; concurrent Fetch calls are safe.
type Downloader struct {
	repo    *Repo
	root    string
	bearer  BearerProvider
	client  *http.Client
	logger  *slog.Logger
	maxSize int64

	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewDownloader constructs a Downloader. root is the absolute or relative
// directory under which per-session subfolders are created.
func NewDownloader(repo *Repo, root string, bearer BearerProvider, logger *slog.Logger) *Downloader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Downloader{
		repo:     repo,
		root:     root,
		bearer:   bearer,
		client:   &http.Client{Timeout: 5 * time.Minute},
		logger:   logger,
		maxSize:  MaxArtifactBytes,
		inflight: make(map[string]struct{}),
	}
}

// Root returns the directory under which artifact files are stored. Exposed
// for diagnostics and tests; callers should not rely on it for routing.
func (d *Downloader) Root() string { return d.root }

// Enqueue spawns a background goroutine that fetches the artifact body and
// updates its row. Returns immediately. If the artifact is already inflight,
// Enqueue is a no-op so we don't double-fetch the same file.
func (d *Downloader) Enqueue(ctx context.Context, a Artifact) {
	d.mu.Lock()
	if _, busy := d.inflight[a.ID]; busy {
		d.mu.Unlock()
		return
	}
	d.inflight[a.ID] = struct{}{}
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.inflight, a.ID)
			d.mu.Unlock()
		}()
		// Use a fresh background context so we don't get cancelled when the
		// HTTP request that triggered the enqueue completes. We do honour a
		// generous overall deadline below.
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := d.Fetch(bg, a); err != nil {
			d.logger.Warn("artifacts: fetch failed",
				"artifact_id", a.ID,
				"session_id", a.SessionID,
				"url", a.RemoteURL,
				"err", err)
		}
	}()
}

// Fetch performs the actual download synchronously. Returns the artifact's
// final on-disk path on success; on failure the row is marked failed and a
// non-nil error is returned.
func (d *Downloader) Fetch(ctx context.Context, a Artifact) error {
	if err := d.repo.MarkDownloading(ctx, a.ID); err != nil {
		return fmt.Errorf("mark downloading: %w", err)
	}
	bearer, err := d.bearer(ctx, a.SessionID)
	if err != nil {
		_ = d.repo.MarkFailed(ctx, a.ID, "reveal key: "+err.Error())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.RemoteURL, nil)
	if err != nil {
		_ = d.repo.MarkFailed(ctx, a.ID, "build request: "+err.Error())
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "devin-key-manager/dev")

	resp, err := d.client.Do(req)
	if err != nil {
		_ = d.repo.MarkFailed(ctx, a.ID, "http: "+err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a short prefix so the diagnostic isn't completely blank.
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		msg := fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
		_ = d.repo.MarkFailed(ctx, a.ID, msg)
		return errors.New(msg)
	}

	contentType := resp.Header.Get("Content-Type")
	dir := filepath.Join(d.root, a.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = d.repo.MarkFailed(ctx, a.ID, "mkdir: "+err.Error())
		return err
	}
	ext := extensionFor(a.Filename, contentType)
	finalPath := filepath.Join(dir, a.ID+ext)
	// Write to a temp file first so a crash mid-download doesn't leave a
	// half-written artifact pretending to be ready.
	tmpPath := finalPath + ".part"
	f, err := os.Create(tmpPath)
	if err != nil {
		_ = d.repo.MarkFailed(ctx, a.ID, "create file: "+err.Error())
		return err
	}
	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, d.maxSize+1)
	n, copyErr := io.Copy(io.MultiWriter(f, hasher), limited)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		_ = d.repo.MarkFailed(ctx, a.ID, "copy: "+copyErr.Error())
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		_ = d.repo.MarkFailed(ctx, a.ID, "close: "+closeErr.Error())
		return closeErr
	}
	if n > d.maxSize {
		_ = os.Remove(tmpPath)
		msg := fmt.Sprintf("artifact exceeds %d bytes", d.maxSize)
		_ = d.repo.MarkFailed(ctx, a.ID, msg)
		return errors.New(msg)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		_ = d.repo.MarkFailed(ctx, a.ID, "rename: "+err.Error())
		return err
	}
	filename := a.Filename
	if filename == "" || filename == a.ID {
		filename = filepath.Base(finalPath)
	}
	if err := d.repo.MarkReady(ctx, a.ID, ReadyInput{
		LocalPath:   finalPath,
		ContentType: contentType,
		SizeBytes:   n,
		SHA256:      hex.EncodeToString(hasher.Sum(nil)),
		Filename:    filename,
	}); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	d.logger.Info("artifacts: downloaded",
		"artifact_id", a.ID,
		"session_id", a.SessionID,
		"path", finalPath,
		"bytes", n,
		"content_type", contentType)
	return nil
}

// extensionFor returns a sensible file extension for the artifact. The
// filename takes precedence if it already has one; otherwise we derive an
// extension from the Content-Type header. Falls back to "" so files without
// an obvious type still work (the browser will sniff at view time).
func extensionFor(filename, contentType string) string {
	if ext := filepath.Ext(filename); ext != "" {
		return ext
	}
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "application/x-msdownload", "application/vnd.microsoft.portable-executable":
		return ".exe"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "application/json":
		return ".json"
	default:
		return ""
	}
}
