package keys_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func newStickyTestRepo(t *testing.T) *keys.Repo {
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

func TestPickWithPreference_HitsPreferred(t *testing.T) {
	r := newStickyTestRepo(t)
	ctx := context.Background()
	a, err := r.Create(ctx, keys.CreateInput{Label: "A", APIKey: "tokenA-1234567890", Plan: keys.PlanFree})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Create(ctx, keys.CreateInput{Label: "B", APIKey: "tokenB-1234567890", Plan: keys.PlanTrial})
	if err != nil {
		t.Fatal(err)
	}
	// Without preference, trial B would win (priority order).
	picked, err := r.PickWithPreference(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != a.ID {
		t.Errorf("expected preferred A (%s), got %s (%s)", a.ID, picked.ID, picked.Label)
	}
}

func TestPickWithPreference_FallsBack(t *testing.T) {
	r := newStickyTestRepo(t)
	ctx := context.Background()
	a, err := r.Create(ctx, keys.CreateInput{Label: "A", APIKey: "tokenA-1234567890", Plan: keys.PlanFree})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Create(ctx, keys.CreateInput{Label: "B", APIKey: "tokenB-1234567890", Plan: keys.PlanTrial})
	if err != nil {
		t.Fatal(err)
	}
	// Put A on cooldown — preference should fail and we should get B.
	until := time.Now().Add(time.Hour)
	if err := r.ApplyCheckOutcome(ctx, a.ID, keys.CheckOutcome{
		State: keys.StateCooldownDaily, Status: "quota_exhausted", CooldownUntil: &until,
	}); err != nil {
		t.Fatal(err)
	}
	picked, err := r.PickWithPreference(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != b.ID {
		t.Errorf("expected fallback to B, got %s", picked.Label)
	}
}

func TestPickWithPreference_EmptyPreference(t *testing.T) {
	r := newStickyTestRepo(t)
	ctx := context.Background()
	_, _ = r.Create(ctx, keys.CreateInput{Label: "A", APIKey: "tokenA-1234567890", Plan: keys.PlanFree})
	picked, err := r.PickWithPreference(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if picked.Label != "A" {
		t.Errorf("expected A, got %s", picked.Label)
	}
}
