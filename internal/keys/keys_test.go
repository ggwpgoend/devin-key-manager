package keys_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func newTestRepo(t *testing.T) *keys.Repo {
	t.Helper()
	dir := t.TempDir()
	c, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return keys.NewRepo(db, c)
}

func TestCreateListGet(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	k, err := repo.Create(ctx, keys.CreateInput{
		Label:  "trial-1",
		Plan:   keys.PlanTrial,
		APIKey: "sk-test-aaaaaaaaaaaaaaaaaaaaaaa",
		Notes:  "first trial",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.ID == "" || k.State != keys.StateActive {
		t.Fatalf("unexpected key: %+v", k)
	}
	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 key, got %d", len(all))
	}
	got, err := repo.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "trial-1" || got.Plan != keys.PlanTrial {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	const sameKey = "sk-test-shared-value"
	if _, err := repo.Create(ctx, keys.CreateInput{Label: "a", Plan: keys.PlanFree, APIKey: sameKey}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := repo.Create(ctx, keys.CreateInput{Label: "b", Plan: keys.PlanPaid, APIKey: sameKey}); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	k, err := repo.Create(ctx, keys.CreateInput{Label: "orig", Plan: keys.PlanFree, APIKey: "sk-x-abc123"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Update(ctx, k.ID, keys.UpdateInput{Label: "renamed", Plan: keys.PlanPaid, Notes: "ok"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := repo.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Label != "renamed" || got.Plan != keys.PlanPaid || got.Notes != "ok" {
		t.Fatalf("update mismatch: %+v", got)
	}
	if err := repo.Delete(ctx, k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, k.ID); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestReveal(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	k, err := repo.Create(ctx, keys.CreateInput{Label: "r", Plan: keys.PlanTrial, APIKey: "sk-reveal-me"})
	if err != nil {
		t.Fatal(err)
	}
	val, err := repo.Reveal(ctx, k.ID)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if val != "sk-reveal-me" {
		t.Fatalf("reveal mismatch: %q", val)
	}
}

func TestFingerprintTrimsWhitespace(t *testing.T) {
	a := keys.Fingerprint("  sk-x-abc  ")
	b := keys.Fingerprint("sk-x-abc")
	if a != b {
		t.Fatalf("fingerprint should ignore surrounding whitespace: %q vs %q", a, b)
	}
}
