package handoffs_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

type fixtures struct {
	repo     *handoffs.Repo
	tasks    *tasks.Repo
	sessions *sessions.Repo
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
		repo:     handoffs.NewRepo(db),
		tasks:    tasks.NewRepo(db),
		sessions: sessions.NewRepo(db),
		keys:     keys.NewRepo(db, c),
	}
}

func TestCreateAndListByTask(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	if _, err := f.repo.Create(ctx, handoffs.CreateInput{Markdown: "x"}); err == nil {
		t.Errorf("expected error when task id is missing")
	}

	task, err := f.tasks.Create(ctx, tasks.CreateInput{Title: "T", Prompt: "p"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	h, err := f.repo.Create(ctx, handoffs.CreateInput{
		TaskID:   task.ID,
		Markdown: "# Hello",
	})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}
	if h.ID == "" || h.Markdown != "# Hello" {
		t.Errorf("unexpected handoff: %+v", h)
	}
	list, err := f.repo.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 handoff, got %d", len(list))
	}
}

func TestLinkToFillsToSessionID(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	task, err := f.tasks.Create(ctx, tasks.CreateInput{Title: "T", Prompt: "p"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	k, err := f.keys.Create(ctx, keys.CreateInput{Label: "k", Plan: keys.PlanTrial, APIKey: "sk-x"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	sess, err := f.sessions.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: k.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h, err := f.repo.Create(ctx, handoffs.CreateInput{TaskID: task.ID, Markdown: "# x"})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}

	if err := f.repo.LinkTo(ctx, h.ID, ""); err == nil {
		t.Errorf("expected error linking to empty session id")
	}
	if err := f.repo.LinkTo(ctx, h.ID, sess.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	got, err := f.repo.GetForSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != h.ID {
		t.Errorf("got %s, want %s", got.ID, h.ID)
	}
}

func TestGetForSessionMissingIsNotFound(t *testing.T) {
	f := newFixtures(t)
	_, err := f.repo.GetForSession(context.Background(), "missing")
	if !errors.Is(err, handoffs.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
