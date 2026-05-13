package artifacts_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
)

func TestDownloaderFetchHappyPath(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)

	body := []byte("\x89PNG\r\n\x1a\nfake png data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing/wrong bearer: %q", got)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	a, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID:    taskID,
		SessionID: sessionID,
		Filename:  "snap.png",
		RemoteURL: server.URL + "/snap.png",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	root := filepath.Join(t.TempDir(), "artifacts")
	d := artifacts.NewDownloader(f.repo, root,
		func(_ context.Context, _ string) (string, error) { return "test-key", nil }, nil)

	if err := d.Fetch(ctx, a); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := f.repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after fetch: %v", err)
	}
	if got.Status != artifacts.StatusReady {
		t.Fatalf("status=%q want ready (error=%q)", got.Status, got.Error)
	}
	if !strings.HasPrefix(got.ContentType, "image/png") {
		t.Errorf("content type=%q, want image/png", got.ContentType)
	}
	if got.SizeBytes != int64(len(body)) {
		t.Errorf("size=%d, want %d", got.SizeBytes, len(body))
	}
	on, err := os.ReadFile(got.LocalPath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(on) != string(body) {
		t.Errorf("on-disk body mismatch")
	}
	// Local path should live under root/<session_id>/
	wantPrefix := filepath.Join(root, sessionID)
	if !strings.HasPrefix(got.LocalPath, wantPrefix) {
		t.Errorf("local path %q not under %q", got.LocalPath, wantPrefix)
	}
}

func TestDownloaderFetchHTTPError(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	a, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID: taskID, SessionID: sessionID, RemoteURL: server.URL + "/x.bin",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	d := artifacts.NewDownloader(f.repo, t.TempDir(),
		func(_ context.Context, _ string) (string, error) { return "key", nil }, nil)
	if err := d.Fetch(ctx, a); err == nil {
		t.Fatalf("expected error, got nil")
	}
	got, _ := f.repo.Get(ctx, a.ID)
	if got.Status != artifacts.StatusFailed {
		t.Errorf("status=%q want failed", got.Status)
	}
	if !strings.Contains(got.Error, "403") {
		t.Errorf("error=%q, expected status code mention", got.Error)
	}
}

func TestDownloaderEnqueueIsAsync(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)

	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello")
	}))
	t.Cleanup(server.Close)

	a, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID: taskID, SessionID: sessionID, RemoteURL: server.URL + "/hello.txt",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d := artifacts.NewDownloader(f.repo, t.TempDir(),
		func(_ context.Context, _ string) (string, error) { return "key", nil }, nil)
	d.Enqueue(ctx, a)

	// Until we release the gate, status should remain pending or downloading.
	time.Sleep(50 * time.Millisecond)
	pre, _ := f.repo.Get(ctx, a.ID)
	if pre.Status != artifacts.StatusPending && pre.Status != artifacts.StatusDownloading {
		t.Errorf("pre-release status=%q, want pending|downloading", pre.Status)
	}
	close(gate)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := f.repo.Get(ctx, a.ID)
		if got.Status == artifacts.StatusReady {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("artifact never reached ready state")
}
