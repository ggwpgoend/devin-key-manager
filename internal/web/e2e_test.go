package web_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// TestEndToEndTaskFlow drives /tasks → POST /tasks → /sessions/{id} →
// /sessions/{id}/messages against an httptest-backed mock Devin API and asserts
// the chat HTML contains both the user prompt and Devin's reply.
func TestEndToEndTaskFlow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cipher, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	type devinState struct {
		messages []devin.Message
	}
	state := &devinState{
		messages: []devin.Message{
			{Type: "user_message", Message: "Please write a Windows GUI for FFmpeg.",
				Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
			{Type: "devin_message", Message: "On it! I'll scaffold a Go + Fyne project, then drive the encoder via go-ffmpeg.",
				Timestamp: time.Date(2026, 1, 1, 12, 0, 7, 0, time.UTC)},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-mock-1","url":"https://app.devin.ai/sessions/devin-mock-1"}`)
	})
	mux.HandleFunc("/session/devin-mock-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "devin-mock-1",
			"status":     "running",
			"messages":   state.messages,
		})
	})
	mux.HandleFunc("/session/devin-mock-1/message", func(w http.ResponseWriter, r *http.Request) {
		var body devin.SendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.messages = append(state.messages, devin.Message{
			Type: "user_message", Message: body.Message,
			Timestamp: time.Date(2026, 1, 1, 12, 0, 15, 0, time.UTC),
		})
		w.WriteHeader(http.StatusNoContent)
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)

	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	factory := func(apiKey string) *devin.Client {
		return devin.NewClient(apiKey, devin.WithBaseURL(mock.URL))
	}
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		ClientFactory: factory,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "trial-mock", Plan: keys.PlanTrial, APIKey: "sk-mock-trial",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	srv, err := web.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{Keys: keysRepo, Tasks: tasksRepo, Sessions: sessionsRepo, Handoffs: handoffsRepo, Manager: mgr},
		filepath.Join(dir, "master.key"),
	)
	if err != nil {
		t.Fatalf("web server: %v", err)
	}
	h := srv.Handler()

	// 1. /tasks initially empty.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/tasks", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("tasks index: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No tasks yet") {
		t.Errorf("empty state missing")
	}

	// 2. POST /tasks creates a task and redirects to its session.
	form := url.Values{}
	form.Set("title", "FFmpeg GUI")
	form.Set("prompt", "Please write a Windows GUI for FFmpeg.")
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create task: %d body: %s", rr.Code, rr.Body.String())
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, "/sessions/") {
		t.Fatalf("redirect: %q", location)
	}
	sessID := strings.TrimPrefix(location, "/sessions/")

	// 3. Sync messages from the mock Devin.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/"+sessID+"/sync", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("sync: %d body: %s", rr.Code, rr.Body.String())
	}

	// 4. GET /sessions/{id} renders the chat page with both messages.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+sessID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("chat page: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		"Please write a Windows GUI for FFmpeg.",
		"go-ffmpeg",
		"Devin",
		"Chat with Devin",
		"devin-mock-1",
	} {
		if !strings.Contains(body, snippet) {
			t.Errorf("chat body missing %q", snippet)
		}
	}

	// 5. POST a follow-up: the partial response must include all three msgs.
	form = url.Values{}
	form.Set("text", "Sounds great — make it dark-themed please.")
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+sessID+"/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("send follow-up: %d body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	if !strings.Contains(body, "dark-themed") {
		t.Errorf("follow-up not in stream: %s", body)
	}
	if !strings.Contains(body, "go-ffmpeg") {
		t.Errorf("earlier devin message dropped: %s", body)
	}
}
