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
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// newTestServerWithFakeDevin returns a handler whose manager talks to a fake
// Devin server: any Authorization header containing "bad" -> 401, anything
// else -> 200. Returns the handler and the keys.Repo so tests can inspect
// what landed in the DB.
func newTestServerWithFakeDevin(t *testing.T) (http.Handler, *keys.Repo) {
	t.Helper()
	devinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}))
	t.Cleanup(devinSrv.Close)

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
	mgr := manager.New(
		keysRepo,
		tasks.NewRepo(db),
		sessions.NewRepo(db),
		handoffs.NewRepo(db),
		manager.Options{
			ClientFactory: func(apiKey string) *devin.Client {
				return devin.NewClient(apiKey, devin.WithBaseURL(devinSrv.URL))
			},
		},
	)
	srv, err := web.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{
			Keys: keysRepo, Tasks: tasks.NewRepo(db),
			Sessions: sessions.NewRepo(db), Handoffs: handoffs.NewRepo(db),
			Manager: mgr,
		},
		"/tmp/test.key",
	)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv.Handler(), keysRepo
}

func TestKeysBulk_DialogRenders(t *testing.T) {
	h, _ := newTestServerWithFakeDevin(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/keys/bulk", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"Bulk import keys", `name="payload"`, "label : plan : key"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("missing %q in dialog HTML", want)
		}
	}
}

func TestKeysBulk_ImportPersists(t *testing.T) {
	h, repo := newTestServerWithFakeDevin(t)
	form := url.Values{"payload": []string{
		"trial-a dev-aaaa-1\n" +
			"trial-b: dev-aaaa-2\n" +
			"team-c : paid : dev-aaaa-3\n" +
			"bad-key dev-bad-1\n",
	}}
	req := httptest.NewRequest(http.MethodPost, "/keys/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Bulk import — results",
		"3 created",
		"1 unauthorized",
		"team-c",
		"bad-key",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in results HTML", want)
		}
	}
	all, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 keys persisted, got %d", len(all))
	}
}

func TestKeysCreate_AutoDetect(t *testing.T) {
	h, repo := newTestServerWithFakeDevin(t)
	form := url.Values{
		"label":   []string{"auto-1"},
		"plan":    []string{"auto"},
		"api_key": []string{"dev-aaaa-auto"},
		"notes":   []string{""},
	}
	req := httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "auto-detected+as+trial") {
		t.Errorf("expected detection flash in redirect, got %q", loc)
	}
	all, _ := repo.List(context.Background())
	if len(all) != 1 || all[0].Plan != keys.PlanTrial {
		t.Errorf("expected one trial key, got %+v", all)
	}
}

func TestKeysCreate_AutoDetect_Unauthorized(t *testing.T) {
	h, repo := newTestServerWithFakeDevin(t)
	form := url.Values{
		"label":   []string{"auto-bad"},
		"plan":    []string{"auto"},
		"api_key": []string{"dev-bad-99"},
	}
	req := httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Auto-detect failure renders the dialog with an error; no redirect.
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unauthorized") {
		t.Errorf("expected unauthorized error in dialog, got: %s", rr.Body.String())
	}
	all, _ := repo.List(context.Background())
	if len(all) != 0 {
		t.Errorf("expected no key persisted, got %d", len(all))
	}
}

func TestKeysDetectPlan_Endpoint(t *testing.T) {
	h, _ := newTestServerWithFakeDevin(t)
	form := url.Values{"api_key": []string{"dev-aaaa-detect"}}
	req := httptest.NewRequest(http.MethodPost, "/keys/detect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Detected: trial") {
		t.Errorf("expected detection chip, got %q", rr.Body.String())
	}

	// Unauthorized path.
	form = url.Values{"api_key": []string{"dev-bad-detect"}}
	req = httptest.NewRequest(http.MethodPost, "/keys/detect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "Unauthorized") {
		t.Errorf("expected unauthorized hint, got %q", rr.Body.String())
	}
}
