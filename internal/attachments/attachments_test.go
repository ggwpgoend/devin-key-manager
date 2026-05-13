package attachments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, tmp)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Seed a parent session row so the FK passes.
	_, err = db.ExecContext(ctx, `INSERT INTO keys (id, label, plan_type, api_key_encrypted, api_key_fingerprint, state, created_at, updated_at) VALUES ('k', 'k', 'trial', '', 'fp', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO tasks (id, title, initial_prompt, status, created_at, updated_at) VALUES ('t', 'demo', 'go', 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, task_id, key_id, status, started_at) VALUES ('s', 't', 'k', 'creating', CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return db
}

func TestFileIOUploader_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"link":"https://file.io/abc123","key":"abc123"}`))
	}))
	defer srv.Close()
	up := &FileIOUploader{Client: srv.Client(), Endpoint: srv.URL}
	url, err := up.Upload(context.Background(), "hello.txt", "text/plain", []byte("hello"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if url != "https://file.io/abc123" {
		t.Fatalf("got %q", url)
	}
}

func TestFileIOUploader_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"message":"boom"}`))
	}))
	defer srv.Close()
	up := &FileIOUploader{Client: srv.Client(), Endpoint: srv.URL}
	if _, err := up.Upload(context.Background(), "f.txt", "text/plain", []byte("x")); err == nil {
		t.Fatalf("expected error on 500")
	}
}

// fakeUploader records calls and returns a canned URL.
type fakeUploader struct {
	url    string
	calls  int
	lastFn string
}

func (f *fakeUploader) Name() string { return "fake" }
func (f *fakeUploader) Upload(_ context.Context, fn, _ string, _ []byte) (string, error) {
	f.calls++
	f.lastFn = fn
	return f.url, nil
}

func TestRepoCreateAndList(t *testing.T) {
	db := openTestDB(t)
	up := &fakeUploader{url: "https://example.com/x"}
	repo := NewRepo(db, up)
	ctx := context.Background()
	att, err := repo.Create(ctx, "s", "hello.txt", "text/plain", []byte("hi"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if att.PublicURL != "https://example.com/x" {
		t.Fatalf("public url: %q", att.PublicURL)
	}
	if att.Status != StatusUploaded {
		t.Fatalf("status %q", att.Status)
	}
	list, err := repo.ListBySession(ctx, "s")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != att.ID {
		t.Fatalf("got %+v", list)
	}
	if up.calls != 1 || up.lastFn != "hello.txt" {
		t.Fatalf("uploader not invoked correctly: %+v", up)
	}
}
