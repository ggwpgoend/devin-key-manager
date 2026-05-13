package keys_test

import (
	"context"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

func TestMarkQuotaExhaustedFirstCycleIsDaily(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	k, err := r.Create(ctx, keys.CreateInput{Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mark, err := r.MarkQuotaExhausted(ctx, k.ID)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if mark.NewState != keys.StateCooldownDaily {
		t.Errorf("state: got %s, want cooldown_daily", mark.NewState)
	}
	if mark.CyclesUsed != 1 {
		t.Errorf("cycles: got %d, want 1", mark.CyclesUsed)
	}
	got, err := r.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CooldownUntil == nil || got.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Errorf("cooldown_until not ~24h from now: %v", got.CooldownUntil)
	}
}

func TestMarkQuotaExhaustedThirdCycleEscalatesToWeekly(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	k, err := r.Create(ctx, keys.CreateInput{Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 2; i++ {
		mark, err := r.MarkQuotaExhausted(ctx, k.ID)
		if err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
		if mark.NewState != keys.StateCooldownDaily {
			t.Fatalf("cycle %d should still be daily, got %s", i, mark.NewState)
		}
	}
	mark, err := r.MarkQuotaExhausted(ctx, k.ID)
	if err != nil {
		t.Fatalf("mark 3: %v", err)
	}
	if mark.NewState != keys.StateCooldownWeekly {
		t.Errorf("3rd cycle: got %s, want cooldown_weekly", mark.NewState)
	}
	got, err := r.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WeekResetAt == nil {
		t.Errorf("week_reset_at should be set")
	}
}

func TestReactivateLiftsExpiredDailyCooldown(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	k, err := r.Create(ctx, keys.CreateInput{Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.MarkQuotaExhausted(ctx, k.ID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// Sneak the cooldown_until into the past so Reactivate picks it up.
	past := time.Now().UTC().Add(-time.Hour)
	if err := r.SetCooldownUntilForTest(ctx, k.ID, past); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	ids, err := r.Reactivate(ctx)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if len(ids) != 1 || ids[0] != k.ID {
		t.Errorf("reactivated: got %v, want [%s]", ids, k.ID)
	}
	got, _ := r.Get(ctx, k.ID)
	if got.State != keys.StateActive {
		t.Errorf("state after reactivate: %s, want active", got.State)
	}
	if got.CooldownUntil != nil {
		t.Errorf("cooldown_until should be cleared, got %v", got.CooldownUntil)
	}
	// Daily cycle counter is preserved across daily reactivations so the
	// weekly cap still kicks in on the 3rd cycle.
	if got.DailyCyclesUsedThisWeek != 1 {
		t.Errorf("cycles: got %d, want 1", got.DailyCyclesUsedThisWeek)
	}
}

func TestReactivateClearsCounterOnWeeklyReset(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	k, err := r.Create(ctx, keys.CreateInput{Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Three quota events → weekly cooldown.
	for i := 0; i < 3; i++ {
		if _, err := r.MarkQuotaExhausted(ctx, k.ID); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := r.SetWeekResetAtForTest(ctx, k.ID, past); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	ids, err := r.Reactivate(ctx)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if len(ids) != 1 || ids[0] != k.ID {
		t.Errorf("reactivated: got %v", ids)
	}
	got, _ := r.Get(ctx, k.ID)
	if got.State != keys.StateActive {
		t.Errorf("state: %s", got.State)
	}
	if got.DailyCyclesUsedThisWeek != 0 {
		t.Errorf("cycles should reset, got %d", got.DailyCyclesUsedThisWeek)
	}
	if got.WeekResetAt != nil {
		t.Errorf("week_reset_at should be cleared, got %v", got.WeekResetAt)
	}
}

func TestMarkQuotaExhaustedRejectsDeadKey(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	k, err := r.Create(ctx, keys.CreateInput{Label: "x", Plan: keys.PlanTrial, APIKey: "sk-x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Apply a check outcome that marks the key dead.
	if err := r.ApplyCheckOutcome(ctx, k.ID, keys.CheckOutcome{State: keys.StateDead, Status: "unauthorized"}); err != nil {
		t.Fatalf("apply check: %v", err)
	}
	if _, err := r.MarkQuotaExhausted(ctx, k.ID); err == nil {
		t.Errorf("expected error marking dead key as quota-exhausted")
	}
}
