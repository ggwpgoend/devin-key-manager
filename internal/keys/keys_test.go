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

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"Work", "work", "WORK"}, "work"},
		{[]string{"  beta  ", "alpha", " "}, "alpha,beta"},
		{[]string{"two words", "comma,bad"}, "commabad,two-words"},
	}
	for _, tc := range cases {
		got := keys.NormalizeTags(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeTags(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetTagsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	k, err := repo.Create(ctx, keys.CreateInput{Label: "tagged", Plan: keys.PlanFree, APIKey: "sk-tag-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTags(ctx, k.ID, []string{"Work", "trial-batch-2025"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	got, err := repo.Get(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tags != "trial-batch-2025,work" {
		t.Fatalf("tags=%q", got.Tags)
	}
	list := got.TagsList()
	if len(list) != 2 || list[0] != "trial-batch-2025" || list[1] != "work" {
		t.Fatalf("tags list=%v", list)
	}
}

func TestDeleteMany(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	var ids []string
	for i := 0; i < 4; i++ {
		k, err := repo.Create(ctx, keys.CreateInput{
			Label:  "b",
			Plan:   keys.PlanTrial,
			APIKey: "sk-bulk-" + string(rune('a'+i)) + "-xxxxxxxxxxxx",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, k.ID)
	}
	n, err := repo.DeleteMany(ctx, ids[:3])
	if err != nil {
		t.Fatalf("delete many: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted=%d want 3", n)
	}
	all, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != ids[3] {
		t.Fatalf("remaining=%+v", all)
	}
	// idempotent on stale ids
	n, err = repo.DeleteMany(ctx, ids[:3])
	if err != nil || n != 0 {
		t.Fatalf("stale delete=%d, err=%v", n, err)
	}
}
