package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

type fixtures struct {
	keys     *keys.Repo
	tasks    *tasks.Repo
	sessions *sessions.Repo
	handoffs *handoffs.Repo
	mgr      *manager.Manager
	server   *httptest.Server
	calls    *atomic.Int32
	handler  *http.ServeMux
}

func newFixtures(t *testing.T) *fixtures {
	t.Helper()
	dir := t.TempDir()
	cipher, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	calls := &atomic.Int32{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)

	factory := func(apiKey string) *devin.Client {
		return devin.NewClient(apiKey, devin.WithBaseURL(srv.URL))
	}
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientFactory: factory,
	})

	return &fixtures{
		keys:     keysRepo,
		tasks:    tasksRepo,
		sessions: sessionsRepo,
		handoffs: handoffsRepo,
		mgr:      mgr,
		server:   srv,
		calls:    calls,
		handler:  mux,
	}
}

func TestStartTaskNoKey(t *testing.T) {
	f := newFixtures(t)
	_, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{Prompt: "hi"})
	if !errors.Is(err, manager.ErrNoActiveKey) {
		t.Fatalf("want ErrNoActiveKey, got %v", err)
	}
}

func TestStartTaskHappyPath(t *testing.T) {
	f := newFixtures(t)
	_, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial-a", Plan: keys.PlanTrial, APIKey: "sk-test-trial",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	var sawAuth, sawBody string
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		sawAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		sawBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-xyz","url":"https://app.devin.ai/sessions/devin-xyz"}`)
	})

	result, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{
		Title:  "Write a Windows GUI",
		Prompt: "Please write a Windows GUI for FFmpeg.",
	})
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	if result.Task.Status != tasks.StatusRunning {
		t.Errorf("task status: %s", result.Task.Status)
	}
	if result.Session.DevinSessionID != "devin-xyz" {
		t.Errorf("devin session id: %q", result.Session.DevinSessionID)
	}
	if result.Session.Status != sessions.StatusRunning {
		t.Errorf("session status: %s", result.Session.Status)
	}
	if sawAuth != "Bearer sk-test-trial" {
		t.Errorf("auth: %q", sawAuth)
	}
	if !strings.Contains(sawBody, "FFmpeg") {
		t.Errorf("body: %q", sawBody)
	}
	msgs, err := f.sessions.ListMessages(context.Background(), result.Session.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != sessions.RoleUser {
		t.Errorf("seeded messages: %+v", msgs)
	}

	// Key should be marked as used (last_used_at non-null).
	all, _ := f.keys.List(context.Background())
	if len(all) != 1 || all[0].LastUsedAt == nil {
		t.Errorf("key not marked used: %+v", all)
	}
}

func TestStartTaskQuotaExhausted(t *testing.T) {
	f := newFixtures(t)
	created, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial", Plan: keys.PlanTrial, APIKey: "sk-empty",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	})
	_, err = f.mgr.StartTask(context.Background(), manager.StartTaskInput{Prompt: "x"})
	if !errors.Is(err, devin.ErrQuotaExhausted) {
		t.Fatalf("want ErrQuotaExhausted, got %v", err)
	}
	// The first-try quota path cooldowns the offending key, attempts a
	// rotate, and because no replacement key is available the task is
	// parked in 'paused' so the user can resume after the cooldown lifts.
	all, _ := f.tasks.List(context.Background())
	if len(all) != 1 || all[0].Status != tasks.StatusPaused {
		t.Errorf("task status: %+v", all)
	}
	gotKey, err := f.keys.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if gotKey.State != keys.StateCooldownDaily {
		t.Errorf("key state: want cooldown_daily, got %s", gotKey.State)
	}
	if gotKey.CooldownUntil == nil {
		t.Errorf("expected cooldown_until set")
	}
}

func TestStartTaskQuotaRotatesToFreshKey(t *testing.T) {
	f := newFixtures(t)
	bad, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial-bad", Plan: keys.PlanTrial, APIKey: "sk-bad",
	})
	if err != nil {
		t.Fatalf("create bad: %v", err)
	}
	good, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial-good", Plan: keys.PlanTrial, APIKey: "sk-good",
	})
	if err != nil {
		t.Fatalf("create good: %v", err)
	}

	// First key (bad) is picked first by virtue of older created_at; the
	// mock returns 402 for it, then 200 for the second attempt under the
	// good key. We distinguish keys by the Authorization header.
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasSuffix(auth, "sk-bad") {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-good"}`)
	})

	result, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{Prompt: "build me a GUI"})
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	if result.Key.ID != good.ID {
		t.Fatalf("expected rotation to good key, got %s", result.Key.ID)
	}
	if result.Session.DevinSessionID != "devin-good" {
		t.Errorf("devin session id on result: %q", result.Session.DevinSessionID)
	}

	badState, _ := f.keys.Get(context.Background(), bad.ID)
	if badState.State != keys.StateCooldownDaily {
		t.Errorf("bad key state: %s", badState.State)
	}
	if badState.CooldownUntil == nil {
		t.Errorf("bad key cooldown_until should be set")
	}

	chain, err := f.handoffs.ListByTask(context.Background(), result.Task.ID)
	if err != nil {
		t.Fatalf("list handoffs: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 handoff row, got %d", len(chain))
	}
	if chain[0].ToSessionID != result.Session.ID {
		t.Errorf("handoff.to_session_id: got %q want %q", chain[0].ToSessionID, result.Session.ID)
	}
	if !strings.Contains(chain[0].Markdown, "build me a GUI") {
		t.Errorf("handoff markdown missing original prompt:\n%s", chain[0].Markdown)
	}
}

func TestSendFollowUpAndSync(t *testing.T) {
	f := newFixtures(t)
	_, err := f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial", Plan: keys.PlanTrial, APIKey: "sk",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Devin "session" state, updated as we go.
	type devinState struct {
		messages []devin.Message
	}
	state := &devinState{
		messages: []devin.Message{
			{Type: "user_message", Message: "ping", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{Type: "devin_message", Message: "pong", Timestamp: time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)},
		},
	}

	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"devin-1"}`)
	})
	f.handler.HandleFunc("/session/devin-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "devin-1",
			"status":     "running",
			"messages":   state.messages,
		})
	})
	f.handler.HandleFunc("/session/devin-1/message", func(w http.ResponseWriter, r *http.Request) {
		var body devin.SendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.messages = append(state.messages, devin.Message{
			Type:      "user_message",
			Message:   body.Message,
			Timestamp: time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC),
		})
		w.WriteHeader(http.StatusNoContent)
	})

	result, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{Prompt: "ping"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Sync: should pull pong + ping from Devin and replace local cache.
	if err := f.mgr.SyncSession(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs, _ := f.sessions.ListMessages(context.Background(), result.Session.ID)
	if len(msgs) != 2 {
		t.Fatalf("messages after sync: %+v", msgs)
	}
	if msgs[1].Role != sessions.RoleAssistant || msgs[1].Content != "pong" {
		t.Errorf("assistant msg: %+v", msgs[1])
	}

	// Follow up.
	if err := f.mgr.SendFollowUp(context.Background(), result.Session.ID, "again"); err != nil {
		t.Fatalf("follow up: %v", err)
	}
	msgs, _ = f.sessions.ListMessages(context.Background(), result.Session.ID)
	if len(msgs) != 3 || msgs[2].Content != "again" {
		t.Errorf("after follow up: %+v", msgs)
	}

	// Sync again: should still have 3 (Devin recorded our follow up too).
	if err := f.mgr.SyncSession(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("sync2: %v", err)
	}
	msgs, _ = f.sessions.ListMessages(context.Background(), result.Session.ID)
	if len(msgs) != 3 {
		t.Errorf("after sync2: %+v", msgs)
	}
}

func TestSyncMapsTerminalStatus(t *testing.T) {
	f := newFixtures(t)
	_, _ = f.keys.Create(context.Background(), keys.CreateInput{
		Label: "trial", Plan: keys.PlanTrial, APIKey: "sk",
	})
	f.handler.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-2"}`)
	})
	f.handler.HandleFunc("/session/devin-2", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-2","status":"completed","messages":[]}`)
	})
	result, err := f.mgr.StartTask(context.Background(), manager.StartTaskInput{Prompt: "x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.mgr.SyncSession(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sess, err := f.sessions.Get(context.Background(), result.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.Status != sessions.StatusCompleted {
		t.Errorf("status: %s", sess.Status)
	}
	if sess.EndedAt == nil {
		t.Errorf("ended_at should be set on terminal status")
	}
}
