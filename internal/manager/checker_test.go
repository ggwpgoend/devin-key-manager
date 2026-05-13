package manager_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

// TestCheckKeyValid asserts a 200 response flips state to active and stamps
// last_check_status = "valid".
func TestCheckKeyValid(t *testing.T) {
	f := newFixtures(t)
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})

	k, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "good", Plan: keys.PlanTrial, APIKey: "sk-good",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := f.mgr.CheckKey(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Status != devin.ValidateValid {
		t.Errorf("status = %s", res.Status)
	}
	if res.NewState != keys.StateActive {
		t.Errorf("new state = %s", res.NewState)
	}
	reloaded, _ := f.keys.Get(context.Background(), k.ID)
	if reloaded.LastCheckStatus != "valid" {
		t.Errorf("persisted status = %q", reloaded.LastCheckStatus)
	}
	if reloaded.LastCheckedAt == nil {
		t.Error("last_checked_at not stamped")
	}
}

// TestCheckKeyUnauthorized asserts 401 marks the key dead.
func TestCheckKeyUnauthorized(t *testing.T) {
	f := newFixtures(t)
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bad token"}`))
	})

	k, _ := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "bad", Plan: keys.PlanTrial, APIKey: "sk-bad",
	})
	res, err := f.mgr.CheckKey(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Status != devin.ValidateUnauthorized {
		t.Errorf("status = %s", res.Status)
	}
	if res.NewState != keys.StateDead {
		t.Errorf("new state = %s", res.NewState)
	}
	reloaded, _ := f.keys.Get(context.Background(), k.ID)
	if reloaded.State != keys.StateDead {
		t.Errorf("persisted state = %s", reloaded.State)
	}
	if !strings.Contains(reloaded.LastCheckError, "unauthorized") {
		t.Errorf("err = %q", reloaded.LastCheckError)
	}
}

// TestCheckKeyQuotaExhausted asserts 402 sets state=cooldown_daily with ~24h
// cooldown_until.
func TestCheckKeyQuotaExhausted(t *testing.T) {
	f := newFixtures(t)
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"detail":"out of acus"}`))
	})

	k, _ := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial-quota", Plan: keys.PlanTrial, APIKey: "sk-quota",
	})
	before := time.Now().UTC()
	res, err := f.mgr.CheckKey(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Status != devin.ValidateQuotaExhausted {
		t.Errorf("status = %s", res.Status)
	}
	if res.NewState != keys.StateCooldownDaily {
		t.Errorf("new state = %s", res.NewState)
	}
	reloaded, _ := f.keys.Get(context.Background(), k.ID)
	if reloaded.CooldownUntil == nil {
		t.Fatal("cooldown_until not set")
	}
	delta := reloaded.CooldownUntil.Sub(before)
	if delta < 23*time.Hour || delta > 25*time.Hour {
		t.Errorf("cooldown delta = %v, want ~24h", delta)
	}
}

// TestCheckKeyNetworkErrorPreservesState ensures a transient transport error
// does NOT downgrade an active key.
func TestCheckKeyNetworkErrorPreservesState(t *testing.T) {
	f := newFixtures(t)
	// Close the upstream so all calls fail at the transport layer.
	f.server.Close()

	k, _ := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "flaky", Plan: keys.PlanFree, APIKey: "sk-flaky",
	})
	res, err := f.mgr.CheckKey(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Status != devin.ValidateNetworkError {
		t.Errorf("status = %s", res.Status)
	}
	if res.NewState != keys.StateActive {
		t.Errorf("state changed to %s on network error", res.NewState)
	}
	reloaded, _ := f.keys.Get(context.Background(), k.ID)
	if reloaded.State != keys.StateActive {
		t.Errorf("persisted state changed: %s", reloaded.State)
	}
	if reloaded.LastCheckError == "" {
		t.Error("last_check_error empty")
	}
}

// TestCheckAllKeysMixed runs CheckAllKeys against three keys with three
// different mock outcomes (valid / unauthorized / quota).
func TestCheckAllKeysMixed(t *testing.T) {
	f := newFixtures(t)
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer sk-good":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "Bearer sk-bad":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer sk-quota":
			w.WriteHeader(http.StatusPaymentRequired)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	for _, in := range []keys.CreateInput{
		{Label: "good", Plan: keys.PlanTrial, APIKey: "sk-good"},
		{Label: "bad", Plan: keys.PlanTrial, APIKey: "sk-bad"},
		{Label: "quota", Plan: keys.PlanFree, APIKey: "sk-quota"},
	} {
		if _, err := f.keys.Create(context.Background(), in); err != nil {
			t.Fatalf("create %s: %v", in.Label, err)
		}
	}

	results, err := f.mgr.CheckAllKeys(context.Background())
	if err != nil {
		t.Fatalf("check all: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	got := map[string]devin.ValidateStatus{}
	for _, r := range results {
		got[r.Label] = r.Status
	}
	want := map[string]devin.ValidateStatus{
		"good":  devin.ValidateValid,
		"bad":   devin.ValidateUnauthorized,
		"quota": devin.ValidateQuotaExhausted,
	}
	for label, expect := range want {
		if got[label] != expect {
			t.Errorf("%s: got %s want %s", label, got[label], expect)
		}
	}
}
