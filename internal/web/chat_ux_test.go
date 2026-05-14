package web_test

import (
	"context"
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
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// newServerAndRepos builds an in-memory server like newTestServer but also
// returns the repos so individual tests can seed rows directly without going
// through the manager / Devin API.
func newServerAndRepos(t *testing.T) (http.Handler, *keys.Repo, *tasks.Repo, *sessions.Repo) {
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
	keysRepo := keys.NewRepo(db, c)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{})
	srv, err := web.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{Keys: keysRepo, Tasks: tasksRepo, Sessions: sessionsRepo, Handoffs: handoffsRepo, Manager: mgr},
		"/tmp/test.key",
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv.Handler(), keysRepo, tasksRepo, sessionsRepo
}

func TestNotesEndpoint_RoundTrip(t *testing.T) {
	h, keysRepo, tasksRepo, sessionsRepo := newServerAndRepos(t)
	ctx := context.Background()

	k, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "notes-test", Plan: keys.PlanTrial, APIKey: "sk-notes-aaaaaaa",
	})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	task, err := tasksRepo.Create(ctx, tasks.CreateInput{Title: "t", Prompt: "p"})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	sess, err := sessionsRepo.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: k.ID})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	form := url.Values{"notes": []string{"call mom · TODO try with smaller prompt"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/notes",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "saved ") {
		t.Errorf("expected 'saved' confirmation, got %q", rr.Body.String())
	}

	got, err := sessionsRepo.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Notes != "call mom · TODO try with smaller prompt" {
		t.Errorf("notes not persisted: %q", got.Notes)
	}

	// Trailing whitespace stripped on save.
	form = url.Values{"notes": []string{"trimmed \n\n\n"}}
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/notes",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	got, _ = sessionsRepo.Get(ctx, sess.ID)
	if got.Notes != "trimmed" {
		t.Errorf("trailing whitespace not stripped: %q", got.Notes)
	}
}

func TestNotesEndpoint_UnknownSession(t *testing.T) {
	h, _, _, _ := newServerAndRepos(t)
	form := url.Values{"notes": []string{"x"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions/no-such-id/notes",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestChatPage_IncludesMarkdownAndNotesPanel(t *testing.T) {
	h, keysRepo, tasksRepo, sessionsRepo := newServerAndRepos(t)
	ctx := context.Background()

	k, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "chat-test", Plan: keys.PlanTrial, APIKey: "sk-chat-aaaaaaa",
	})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	task, err := tasksRepo.Create(ctx, tasks.CreateInput{Title: "t", Prompt: "p"})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	sess, err := sessionsRepo.Create(ctx, sessions.CreateInput{TaskID: task.ID, KeyID: k.ID})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := sessionsRepo.AppendMessage(ctx, sess.ID, sessions.RoleAssistant,
		"Here is the **fix**:\n\n```go\nfmt.Println(\"hi\")\n```",
		time.Now().UTC()); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := sessionsRepo.SetNotes(ctx, sess.ID, "remember to try X"); err != nil {
		t.Fatalf("set notes: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+sess.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	wants := []string{
		// markdown libs are loaded
		"marked.min.js",
		"highlight.min.js",
		// chat bubble carries the data-md marker for client-side render
		`data-md`,
		// raw markdown source must reach the page (client-side render will handle it).
		// html/template escapes quotes as &#34; so we look for the escaped form.
		"```go",
		`fmt.Println(&#34;hi&#34;)`,
		// notes panel rendered with previously-saved notes and open
		"private to you, never sent to Devin",
		"remember to try X",
		`hx-post="/sessions/` + sess.ID + `/notes"`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q", w)
		}
	}
}
