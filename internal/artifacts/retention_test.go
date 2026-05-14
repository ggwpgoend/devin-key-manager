package artifacts_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func newRetentionRepo(t *testing.T) (*artifacts.Repo, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Need a session so the FK on artifacts.session_id is satisfied.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO tasks (id, title, initial_prompt, status, created_at)
		 VALUES ('t1', 'test', 'p', 'pending', ?)`,
		time.Now().UTC()); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return artifacts.NewRepo(db), db
}

func writeFile(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer f.Close()
	if size > 0 {
		buf := make([]byte, size)
		if _, err := f.Write(buf); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return p
}

func TestPrune_DeletesOldUnpinned(t *testing.T) {
	r, db := newRetentionRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	old1 := writeFile(t, dir, "old1.txt", 100)
	pinPath := writeFile(t, dir, "pin.txt", 200)

	now := time.Now().UTC()
	_, err := db.ExecContext(ctx,
		`INSERT INTO artifacts (id, task_id, filename, local_path, devin_url, sha256, size_bytes, content_type, source, status, error, created_at, pinned)
		 VALUES ('a1', 't1', 'old1.txt', ?, 'u1', '', 100, '', 'devin', 'ready', '', ?, 0)`,
		old1, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("insert a1: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO artifacts (id, task_id, filename, local_path, devin_url, sha256, size_bytes, content_type, source, status, error, created_at, pinned)
		 VALUES ('a2', 't1', 'pin.txt', ?, 'u2', '', 200, '', 'devin', 'ready', '', ?, 1)`,
		pinPath, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatalf("insert a2: %v", err)
	}

	res, err := r.Prune(ctx, artifacts.PruneOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Candidates != 1 || res.Deleted != 1 || res.FreedBytes != 100 {
		t.Errorf("expected 1 candidate / 1 deleted / 100 bytes, got %+v", res)
	}
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Errorf("expected old1 to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(pinPath); err != nil {
		t.Errorf("expected pinned file to remain, stat err=%v", err)
	}
}

func TestPrune_DryRun(t *testing.T) {
	r, db := newRetentionRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := writeFile(t, dir, "x.txt", 50)
	_, err := db.ExecContext(ctx,
		`INSERT INTO artifacts (id, task_id, filename, local_path, devin_url, sha256, size_bytes, content_type, source, status, error, created_at, pinned)
		 VALUES ('a', 't1', 'x.txt', ?, 'u', '', 50, '', 'devin', 'ready', '', ?, 0)`,
		p, time.Now().UTC().Add(-100*time.Hour))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := r.Prune(ctx, artifacts.PruneOptions{MaxAge: time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 1 || res.Deleted != 0 {
		t.Errorf("dry run should not delete, got %+v", res)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file should remain after dry run: %v", err)
	}
}

func TestSetPinned(t *testing.T) {
	r, db := newRetentionRepo(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO artifacts (id, task_id, filename, local_path, devin_url, sha256, size_bytes, content_type, source, status, error, created_at, pinned)
		 VALUES ('a', 't1', 'x', '', 'u', '', 0, '', 'devin', 'ready', '', ?, 0)`,
		time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPinned(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	var pinned int
	if err := db.QueryRowContext(ctx, `SELECT pinned FROM artifacts WHERE id = 'a'`).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != 1 {
		t.Errorf("expected pinned=1, got %d", pinned)
	}
}
