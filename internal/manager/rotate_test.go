package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

// TestPollerQuotaTriggersHandoff drives the full quota-handoff loop: an
// active session pings the mock Devin API which returns 402, the manager
// cooldowns the offending key, opens a fresh session on the second key, and
// links the two sessions via the handoffs table.
func TestPollerQuotaTriggersHandoff(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	badKey, err := f.keys.Create(ctx, keys.CreateInput{Label: "bad", Plan: keys.PlanTrial, APIKey: "sk-bad"})
	if err != nil {
		t.Fatalf("create bad: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // ensure different created_at
	goodKey, err := f.keys.Create(ctx, keys.CreateInput{Label: "good", Plan: keys.PlanTrial, APIKey: "sk-good"})
	if err != nil {
		t.Fatalf("create good: %v", err)
	}

	// The mock Devin server: bad key gets 200 for create, but 402 for any
	// subsequent get. Good key gets 200 for everything.
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.Header.Get("Authorization"), "sk-bad") {
			_, _ = io.WriteString(w, `{"session_id":"devin-bad"}`)
		} else {
			_, _ = io.WriteString(w, `{"session_id":"devin-good"}`)
		}
	})
	f.handler.HandleFunc("/session/devin-bad", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	})
	f.handler.HandleFunc("/session/devin-good", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "devin-good",
			"status":     "running",
			"messages":   []devin.Message{{Type: "user_message", Message: "hi"}},
		})
	})

	result, err := f.mgr.StartTask(ctx, manager.StartTaskInput{Prompt: "do the thing", Title: "thing"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.Key.ID != badKey.ID {
		t.Fatalf("expected first session on bad key, got %s", result.Key.ID)
	}

	// Now the poller-style sync should trigger the rotation.
	if err := f.mgr.SyncSession(ctx, result.Session.ID); !errors.Is(err, devin.ErrQuotaExhausted) {
		t.Fatalf("sync: want ErrQuotaExhausted, got %v", err)
	}

	// The bad key should be on cooldown.
	bs, _ := f.keys.Get(ctx, badKey.ID)
	if bs.State != keys.StateCooldownDaily {
		t.Errorf("bad key state: got %s, want cooldown_daily", bs.State)
	}

	// A handoff row should exist linking the dying session to a fresh one.
	chain, err := f.handoffs.ListByTask(ctx, result.Task.ID)
	if err != nil {
		t.Fatalf("list handoffs: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("handoff rows: %d (want 1) — %+v", len(chain), chain)
	}
	if chain[0].FromSessionID != result.Session.ID {
		t.Errorf("from_session_id: got %s, want %s", chain[0].FromSessionID, result.Session.ID)
	}
	if chain[0].ToSessionID == "" {
		t.Fatal("to_session_id should be filled")
	}
	if !strings.Contains(chain[0].Markdown, "do the thing") {
		t.Errorf("handoff markdown missing original prompt:\n%s", chain[0].Markdown)
	}

	// New session should be open on the good key.
	newSess, err := f.sessions.Get(ctx, chain[0].ToSessionID)
	if err != nil {
		t.Fatalf("get new session: %v", err)
	}
	if newSess.KeyID != goodKey.ID {
		t.Errorf("new session key: got %s, want %s", newSess.KeyID, goodKey.ID)
	}
	if newSess.DevinSessionID != "devin-good" {
		t.Errorf("new session devin id: %q", newSess.DevinSessionID)
	}

	// Old session should be in handoff_done.
	oldSess, _ := f.sessions.Get(ctx, result.Session.ID)
	if oldSess.Status != sessions.StatusHandoffDone {
		t.Errorf("old session status: %s, want handoff_done", oldSess.Status)
	}

	// Inbound handoff lookup should resolve.
	inbound, err := f.handoffs.GetForSession(ctx, newSess.ID)
	if err != nil {
		t.Fatalf("get for session: %v", err)
	}
	if inbound.ID != chain[0].ID {
		t.Errorf("inbound id mismatch")
	}
}

func TestForceRotateManualPath(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	_, err := f.keys.Create(ctx, keys.CreateInput{Label: "k1", Plan: keys.PlanTrial, APIKey: "sk-1"})
	if err != nil {
		t.Fatalf("k1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, err = f.keys.Create(ctx, keys.CreateInput{Label: "k2", Plan: keys.PlanTrial, APIKey: "sk-2"})
	if err != nil {
		t.Fatalf("k2: %v", err)
	}
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-x"}`)
	})

	result, err := f.mgr.StartTask(ctx, manager.StartTaskInput{Prompt: "task", Title: "t"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	rotated, err := f.mgr.ForceRotate(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("force rotate: %v", err)
	}
	if rotated.ToSession.ID == result.Session.ID {
		t.Fatal("force rotate should mint a new session")
	}
	if rotated.NewKey.ID == result.Key.ID {
		t.Fatal("force rotate should pick a different key")
	}
	old, _ := f.sessions.Get(ctx, result.Session.ID)
	if old.Status != sessions.StatusHandoffDone {
		t.Errorf("old status: %s", old.Status)
	}
	// Manual rotation should NOT cooldown the previous key — the user just
	// wanted to start fresh.
	oldKey, _ := f.keys.Get(ctx, result.Key.ID)
	if oldKey.State != keys.StateActive {
		t.Errorf("manual rotation should not cooldown the old key, got state %s", oldKey.State)
	}
}

func TestForceRotateOnTerminalSession(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	_, err := f.keys.Create(ctx, keys.CreateInput{Label: "k", Plan: keys.PlanTrial, APIKey: "sk"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-x"}`)
	})
	result, err := f.mgr.StartTask(ctx, manager.StartTaskInput{Prompt: "task", Title: "t"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Mark the session completed manually.
	if err := f.sessions.SetStatus(ctx, result.Session.ID, sessions.StatusCompleted, "manual"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if _, err := f.mgr.ForceRotate(ctx, result.Session.ID); err == nil {
		t.Errorf("expected error rotating a completed session")
	}
}

// TestBuildHandoffMarkdownIncludesEverything is a focused snapshot of the
// markdown produced for a non-trivial conversation, so future template
// changes are explicit.
func TestBuildHandoffMarkdownIncludesEverything(t *testing.T) {
	task := tasks.Task{
		ID:            "task-1",
		Title:         "Build the GUI",
		InitialPrompt: "please build a GUI",
		CreatedAt:     time.Date(2026, 5, 13, 16, 0, 0, 0, time.UTC),
	}
	dying := sessions.Session{
		ID:             "sess-old",
		TaskID:         task.ID,
		KeyID:          "k-old",
		DevinSessionID: "devin-old",
		Status:         sessions.StatusRunning,
	}
	msgs := []sessions.Message{
		{Role: sessions.RoleUser, Content: "hello", Timestamp: time.Date(2026, 5, 13, 17, 0, 0, 0, time.UTC)},
		{Role: sessions.RoleAssistant, Content: "Hi!", Timestamp: time.Date(2026, 5, 13, 17, 0, 5, 0, time.UTC)},
		{Role: sessions.RoleSystem, Content: "Resumed from previous session (XXX). Reason: foo.", Timestamp: time.Date(2026, 5, 13, 17, 0, 10, 0, time.UTC)},
	}
	got := manager.BuildHandoffMarkdown(task, dying, msgs, manager.ReasonQuotaExhausted, "402", time.Date(2026, 5, 13, 17, 5, 0, 0, time.UTC))
	want := []string{
		"# Handoff from previous session",
		"Build the GUI",
		"```",
		"please build a GUI",
		"Conversation history",
		"User",
		"Devin",
		"System",
		"What to do next",
	}
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("handoff missing %q\nmarkdown:\n%s", s, got)
		}
	}
}
