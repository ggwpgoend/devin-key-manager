package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
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

// TestArtifactsEndToEnd drives the full PR-4 flow against an httptest mock:
//  1. Spin up a fake Devin API that returns an assistant message containing
//     an https URL to a fake attachment server.
//  2. Spin up a fake attachment server that serves a small PNG.
//  3. POST /tasks, then sync the session — the manager should extract the URL,
//     create an artifacts row, and the downloader should fetch the file.
//  4. GET /sessions/{id}/files renders the gallery with the artifact.
//  5. GET /artifacts/{id}/raw streams the file body with image/png.
//  6. POST /sessions/{id}/snap returns a 303 (no-op without quota since the
//     mock accepts it) and the gallery page still renders.
func TestArtifactsEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Fake attachment server returning a tiny PNG.
	pngBody := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'd', 'a', 't', 'a'}
	attachSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("downloader missing bearer: %q", got)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBody)
	}))
	t.Cleanup(attachSrv.Close)

	pngURL := attachSrv.URL + "/desktop.png"

	devinState := struct {
		messages []devin.Message
	}{
		messages: []devin.Message{
			{Type: "user_message", Message: "Snap the desktop please.",
				Timestamp: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)},
			{Type: "devin_message",
				Message:   "Sure, here it is: " + pngURL,
				Timestamp: time.Date(2026, 1, 1, 9, 0, 5, 0, time.UTC)},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-mock-art","url":"https://app.devin.ai/sessions/devin-mock-art"}`)
	})
	mux.HandleFunc("/session/devin-mock-art", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "devin-mock-art",
			"status":     "running",
			"messages":   devinState.messages,
		})
	})
	mux.HandleFunc("/session/devin-mock-art/message", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	devinSrv := httptest.NewServer(mux)
	t.Cleanup(devinSrv.Close)

	cipher, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	artRepo := artifacts.NewRepo(db)

	clientFactory := func(apiKey string) *devin.Client {
		return devin.NewClient(apiKey, devin.WithBaseURL(devinSrv.URL))
	}
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientFactory: clientFactory,
		Artifacts:     artRepo,
	})
	downloader := artifacts.NewDownloader(artRepo, filepath.Join(dir, "artifacts"),
		mgr.BearerForSession, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetDownloader(downloader)

	if _, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "trial-art", Plan: keys.PlanTrial, APIKey: "sk-mock-art",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	srv, err := web.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{
			Keys: keysRepo, Tasks: tasksRepo, Sessions: sessionsRepo,
			Handoffs: handoffsRepo, Artifacts: artRepo, Manager: mgr,
		},
		filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("web server: %v", err)
	}
	h := srv.Handler()

	// 1. Create a task via POST /tasks.
	formBody := "title=Snap+demo&prompt=Snap+the+desktop+please."
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(formBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create task: %d body: %s", rr.Code, rr.Body.String())
	}
	sessID := strings.TrimPrefix(rr.Header().Get("Location"), "/sessions/")

	// 2. Sync. This triggers URL extraction → artifact rows + downloader.Enqueue.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/"+sessID+"/sync", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("sync: %d body: %s", rr.Code, rr.Body.String())
	}

	// 3. Wait briefly for the async download to land.
	var aFinal artifacts.Artifact
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := artRepo.ListBySession(ctx, sessID)
		if err != nil {
			t.Fatalf("list artifacts: %v", err)
		}
		if len(list) == 1 && list[0].Status == artifacts.StatusReady {
			aFinal = list[0]
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if aFinal.ID == "" {
		t.Fatalf("artifact never reached ready state")
	}
	if aFinal.RemoteURL != pngURL {
		t.Errorf("remote url=%q want %q", aFinal.RemoteURL, pngURL)
	}
	if !strings.HasPrefix(aFinal.ContentType, "image/png") {
		t.Errorf("content type=%q want image/png", aFinal.ContentType)
	}
	if !aFinal.IsImage() {
		t.Errorf("IsImage() false but should be true")
	}

	// 4. /sessions/{id}/files page mentions the filename and renders an <img>.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+sessID+"/files", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("files: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		"desktop.png",
		`<img src="/artifacts/` + aFinal.ID + `/raw"`,
		"Download",
		"ready",
	} {
		if !strings.Contains(body, snippet) {
			t.Errorf("files page missing %q", snippet)
		}
	}

	// 5. GET /artifacts/{id}/raw streams the file with image/png.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+aFinal.ID+"/raw", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("raw: %d body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("raw content-type=%q want image/png prefix", ct)
	}
	if got := rr.Body.Bytes(); string(got) != string(pngBody) {
		t.Errorf("raw body mismatch")
	}

	// 6. GET /artifacts/{id}/download sets Content-Disposition.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifacts/"+aFinal.ID+"/download", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("download: %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("download missing Content-Disposition: %q", got)
	}

	// 7. POST /sessions/{id}/snap redirects.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/"+sessID+"/snap", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("snap: %d body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "screenshot") {
		t.Errorf("snap redirect=%q, expected flash about screenshot", loc)
	}

	// 8. Chat page now shows the inline <img> in Devin's bubble.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+sessID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("chat: %d body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`/artifacts/%s/raw`, aFinal.ID)) {
		t.Errorf("chat body missing inline image ref to %s", aFinal.ID)
	}
	if !strings.Contains(body, "Snap desktop") {
		t.Errorf("chat body missing Snap desktop button")
	}
	if !strings.Contains(body, `href="/sessions/`+sessID+`/files"`) {
		t.Errorf("chat body missing Files link")
	}
}
