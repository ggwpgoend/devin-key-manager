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

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

func newTestServer(t *testing.T) http.Handler {
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
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, manager.Options{})
	srv, err := web.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{Keys: keysRepo, Tasks: tasksRepo, Sessions: sessionsRepo, Manager: mgr},
		"/tmp/test.key",
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv.Handler()
}

func TestKeysIndexEmpty(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, s := range []string{"No keys yet", "Add key", "API keys", "devin-key-manager"} {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q in body", s)
		}
	}
	if strings.Contains(body, "template clone") {
		t.Error("template clone error leaked into response")
	}
}

// TestKeysIndexAfterPartial reproduces the bug from PR-1 where the layout
// template's parse tree was shared with partial renders. Once any partial was
// served, subsequent page renders failed because Clone() rejects already-
// executed templates. After the refactor the same handler set is used many
// times across both pages and partials.
func TestKeysIndexAfterPartial(t *testing.T) {
	h := newTestServer(t)

	// Render a partial first — under the buggy implementation this used to
	// execute the shared template set, breaking later Clone() calls.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/keys/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("dialog status: %d body: %s", rr.Code, rr.Body.String())
	}

	// Then render the page — this must still succeed.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("index after partial status: %d body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "API keys") {
		t.Errorf("page body missing expected content: %s", rr.Body.String())
	}
}

func TestKeysCreateAndList(t *testing.T) {
	h := newTestServer(t)

	form := url.Values{}
	form.Set("label", "trial-1")
	form.Set("plan", "trial")
	form.Set("api_key", "sk-test-fake-abc-123")
	form.Set("notes", "ok")

	req := httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create status: %d body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); !strings.HasPrefix(got, "/keys?flash=") {
		t.Errorf("unexpected redirect: %q", got)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/keys?flash=Key+added.", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "trial-1") {
		t.Errorf("list missing label, body: %s", body)
	}
	if !strings.Contains(body, "Key added") {
		t.Errorf("list missing flash message")
	}
}

func TestHealthz(t *testing.T) {
	h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("healthz failed: %d %q", rr.Code, rr.Body.String())
	}
}
