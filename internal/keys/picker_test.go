package keys_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

func mustCreate(t *testing.T, r *keys.Repo, label string, plan keys.Plan, apiKey string) keys.Key {
	t.Helper()
	k, err := r.Create(context.Background(), keys.CreateInput{Label: label, Plan: plan, APIKey: apiKey})
	if err != nil {
		t.Fatalf("create %s: %v", label, err)
	}
	return k
}

func TestPickEmpty(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Pick(context.Background()); !errors.Is(err, keys.ErrNoActiveKey) {
		t.Fatalf("want ErrNoActiveKey, got %v", err)
	}
}

func TestPickPrefersTrialOverFreeOverPaid(t *testing.T) {
	r := newTestRepo(t)
	mustCreate(t, r, "paid", keys.PlanPaid, "sk-paid-1")
	mustCreate(t, r, "free", keys.PlanFree, "sk-free-1")
	trial := mustCreate(t, r, "trial", keys.PlanTrial, "sk-trial-1")

	got, err := r.Pick(context.Background())
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.ID != trial.ID {
		t.Fatalf("want trial, got %s (%s)", got.Label, got.Plan)
	}
}

func TestPickRoundRobinsWithinPlan(t *testing.T) {
	r := newTestRepo(t)
	a := mustCreate(t, r, "a", keys.PlanTrial, "sk-a")
	b := mustCreate(t, r, "b", keys.PlanTrial, "sk-b")

	// First pick: both never used, ordered by created_at; "a" wins (older).
	first, err := r.Pick(context.Background())
	if err != nil {
		t.Fatalf("pick1: %v", err)
	}
	if first.ID != a.ID {
		t.Fatalf("first pick: want %s, got %s", a.ID, first.ID)
	}
	if err := r.MarkUsed(context.Background(), first.ID); err != nil {
		t.Fatalf("mark used: %v", err)
	}

	// Second pick: a was just used, b still has NULL last_used_at and so wins
	// (the picker prefers never-used keys over recently-used ones).
	second, err := r.Pick(context.Background())
	if err != nil {
		t.Fatalf("pick2: %v", err)
	}
	if second.ID != b.ID {
		t.Fatalf("second pick: want %s, got %s", b.ID, second.ID)
	}
	if err := r.MarkUsed(context.Background(), second.ID); err != nil {
		t.Fatalf("mark used: %v", err)
	}

	// Third pick: both have a non-null last_used_at; a is older, so wins.
	third, err := r.Pick(context.Background())
	if err != nil {
		t.Fatalf("pick3: %v", err)
	}
	if third.ID != a.ID {
		t.Fatalf("third pick: want %s, got %s", a.ID, third.ID)
	}

	// Sanity: last_used_at on first is older than on second.
	all, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var aLast, bLast time.Time
	for _, k := range all {
		switch k.ID {
		case a.ID:
			if k.LastUsedAt == nil {
				t.Fatal("a.LastUsedAt nil")
			}
			aLast = *k.LastUsedAt
		case b.ID:
			if k.LastUsedAt == nil {
				t.Fatal("b.LastUsedAt nil")
			}
			bLast = *k.LastUsedAt
		}
	}
	if !aLast.Before(bLast) {
		t.Errorf("expected a (%v) used before b (%v)", aLast, bLast)
	}
}

func TestMarkUsedReturnsNotFound(t *testing.T) {
	r := newTestRepo(t)
	if err := r.MarkUsed(context.Background(), "missing"); !errors.Is(err, keys.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
