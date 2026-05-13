package artifacts_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

type fixtures struct {
	db       *store.DB
	repo     *artifacts.Repo
	sessions *sessions.Repo
	tasks    *tasks.Repo
	keys     *keys.Repo
}

func newFixtures(t *testing.T) fixtures {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return fixtures{
		db:       db,
		repo:     artifacts.NewRepo(db),
		sessions: sessions.NewRepo(db),
		tasks:    tasks.NewRepo(db),
		keys:     keys.NewRepo(db, c),
	}
}

func seedSession(t *testing.T, f fixtures) (taskID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	task, err := f.tasks.Create(ctx, tasks.CreateInput{Title: "T", Prompt: "p"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	k, err := f.keys.Create(ctx, keys.CreateInput{Label: "k", Plan: keys.PlanTrial, APIKey: "sk-x"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	s, err := f.sessions.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: k.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return task.ID, s.ID
}

func TestCreateDedupesPerSessionURL(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)

	first, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID:    taskID,
		SessionID: sessionID,
		Filename:  "foo.png",
		RemoteURL: "https://example.com/foo.png",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.Status != artifacts.StatusPending {
		t.Errorf("status=%q, want pending", first.Status)
	}
	dup, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID:    taskID,
		SessionID: sessionID,
		Filename:  "foo.png",
		RemoteURL: "https://example.com/foo.png",
	})
	if !errors.Is(err, artifacts.ErrAlreadyExists) {
		t.Errorf("want ErrAlreadyExists, got %v", err)
	}
	if dup.ID != first.ID {
		t.Errorf("dedup should return first row id, got %s want %s", dup.ID, first.ID)
	}
}

func TestMarkReadyUpdatesMetadata(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)
	a, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID:    taskID,
		SessionID: sessionID,
		Filename:  "snap.png",
		RemoteURL: "https://example.com/snap.png",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.repo.MarkReady(ctx, a.ID, artifacts.ReadyInput{
		LocalPath:   "/tmp/snap.png",
		ContentType: "image/png",
		SizeBytes:   123,
		SHA256:      "deadbeef",
		Filename:    "snap.png",
	}); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	got, err := f.repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != artifacts.StatusReady {
		t.Errorf("status=%q, want ready", got.Status)
	}
	if !got.IsImage() {
		t.Errorf("IsImage() = false, want true (content-type image/png)")
	}
	if got.LocalPath != "/tmp/snap.png" || got.SizeBytes != 123 || got.SHA256 != "deadbeef" {
		t.Errorf("metadata not persisted: %+v", got)
	}
}

func TestMarkFailedRecordsError(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)
	a, err := f.repo.Create(ctx, artifacts.CreateInput{
		TaskID:    taskID,
		SessionID: sessionID,
		RemoteURL: "https://example.com/x.bin",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.repo.MarkFailed(ctx, a.ID, "http 403"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := f.repo.Get(ctx, a.ID)
	if got.Status != artifacts.StatusFailed || got.Error != "http 403" {
		t.Errorf("status=%q error=%q, want failed/http 403", got.Status, got.Error)
	}
}

func TestListBySessionAndTask(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	taskID, sessionID := seedSession(t, f)
	for _, u := range []string{"https://a.example/1", "https://a.example/2", "https://a.example/3"} {
		if _, err := f.repo.Create(ctx, artifacts.CreateInput{
			TaskID: taskID, SessionID: sessionID, RemoteURL: u,
		}); err != nil {
			t.Fatalf("create %s: %v", u, err)
		}
	}
	list, err := f.repo.ListBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list session: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("listBySession len=%d, want 3", len(list))
	}
	tlist, err := f.repo.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("list task: %v", err)
	}
	if len(tlist) != 3 {
		t.Errorf("listByTask len=%d, want 3", len(tlist))
	}
}
