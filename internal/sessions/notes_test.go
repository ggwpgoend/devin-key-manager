package sessions_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

func newSessionsFixture(t *testing.T) (*sessions.Repo, sessions.Session) {
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
	keyRepo := keys.NewRepo(db, c)
	k, err := keyRepo.Create(context.Background(), keys.CreateInput{
		Label: "test", Plan: keys.PlanTrial, APIKey: "sk-test-aaaaaaaa",
	})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	taskRepo := tasks.NewRepo(db)
	tk, err := taskRepo.Create(context.Background(), tasks.CreateInput{
		Title: "t", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	sr := sessions.NewRepo(db)
	sess, err := sr.Create(context.Background(), sessions.CreateInput{
		TaskID: tk.ID, KeyID: k.ID,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return sr, sess
}

func TestSetNotes_RoundTrip(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)

	got, err := repo.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Notes != "" {
		t.Fatalf("expected empty notes on fresh session, got %q", got.Notes)
	}

	if err := repo.SetNotes(ctx, sess.ID, "remember to retry with bigger context"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = repo.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.Notes != "remember to retry with bigger context" {
		t.Fatalf("notes mismatch: %q", got.Notes)
	}

	if err := repo.SetNotes(ctx, sess.ID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = repo.Get(ctx, sess.ID)
	if got.Notes != "" {
		t.Fatalf("expected cleared notes, got %q", got.Notes)
	}
}

func TestSetNotes_UnknownSession(t *testing.T) {
	repo, _ := newSessionsFixture(t)
	err := repo.SetNotes(context.Background(), "nope", "x")
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSetNotes_SurvivesListByTask(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)
	if err := repo.SetNotes(ctx, sess.ID, "abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	list, err := repo.ListByTask(ctx, sess.TaskID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Notes != "abc" {
		t.Fatalf("expected notes preserved across list, got %+v", list)
	}
}
