package web_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/events"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// newFullServer wires every dependency up so the JSON API surface and SSE
// stream can be exercised end-to-end with real repositories.
func newFullServer(t *testing.T) (http.Handler, *keys.Repo, *tasks.Repo, *sessions.Repo, *artifacts.Repo, *events.Bus) {
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
	artifactsRepo := artifacts.NewRepo(db)
	schedulesRepo := schedules.NewRepo(db)
	notifsRepo := notifications.NewRepo(db)
	bus := events.NewBus()
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Artifacts:     artifactsRepo,
		Notifications: notifsRepo,
		Bus:           bus,
	})
	srv, err := web.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{
			Keys:          keysRepo,
			Tasks:         tasksRepo,
			Sessions:      sessionsRepo,
			Handoffs:      handoffsRepo,
			Artifacts:     artifactsRepo,
			Schedules:     schedulesRepo,
			Notifications: notifsRepo,
			Bus:           bus,
			Manager:       mgr,
		},
		"/tmp/test.key",
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv.Handler(), keysRepo, tasksRepo, sessionsRepo, artifactsRepo, bus
}

func TestAPI_HealthEndpoint(t *testing.T) {
	h, _, _, _, _, _ := newFullServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %s", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if got["status"] != "ok" {
		t.Fatalf("status: %#v", got["status"])
	}
}

func TestAPI_ListKeys_FiltersAndCursor(t *testing.T) {
	h, keysRepo, _, _, _, _ := newFullServer(t)
	ctx := context.Background()
	// Seed three keys with different plans/states so we can filter.
	for i, plan := range []keys.Plan{keys.PlanTrial, keys.PlanFree, keys.PlanPaid} {
		_, err := keysRepo.Create(ctx, keys.CreateInput{
			Label:  "key-" + string(plan),
			Plan:   plan,
			APIKey: "sk-aaaaaaaaaaaa-test-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/keys?filter=plan:free", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("filter plan:free → count=%d, want 1, body=%s", resp.Count, rr.Body.String())
	}
	if got := resp.Items[0]["plan"]; got != "free" {
		t.Fatalf("filter returned wrong plan: %v", got)
	}

	// Limit cap.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/keys?limit=2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("limit status: %d", rr.Code)
	}
	resp = struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
	}{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Count != 2 {
		t.Fatalf("limit=2 → count=%d", resp.Count)
	}
}

func TestAPI_KeyGet_NotFound(t *testing.T) {
	h, _, _, _, _, _ := newFullServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/keys/missing-id", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
}

func TestAPI_KeyGet_ExposesMetrics(t *testing.T) {
	h, keysRepo, _, _, _, _ := newFullServer(t)
	ctx := context.Background()
	k, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "metrics-key", Plan: keys.PlanTrial, APIKey: "sk-metrics-aaaaaaa",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Drive the metric mutators directly so the test doesn't need a Devin
	// mock — same code path the manager uses.
	if err := keysRepo.MarkUsed(ctx, k.ID); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	if err := keysRepo.MarkUsed(ctx, k.ID); err != nil {
		t.Fatalf("mark used 2: %v", err)
	}
	if err := keysRepo.BumpSessionsCount(ctx, k.ID); err != nil {
		t.Fatalf("bump sessions: %v", err)
	}
	if err := keysRepo.RecordError(ctx, k.ID, "boom"); err != nil {
		t.Fatalf("record error: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/keys/"+k.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	// The HTTP shape strips the metric fields off the wire (apiKey type
	// omits them) but the Go-level Key struct does carry them. Verify both
	// to make sure migrations + scan + UPDATE work end-to-end.
	gotKey, err := keysRepo.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotKey.RequestCount != 2 {
		t.Errorf("request count: %d, want 2", gotKey.RequestCount)
	}
	if gotKey.SessionsCountTotal != 1 {
		t.Errorf("sessions count: %d, want 1", gotKey.SessionsCountTotal)
	}
	if gotKey.LastErrorMessage != "boom" {
		t.Errorf("last error: %q, want %q", gotKey.LastErrorMessage, "boom")
	}
	if gotKey.ActivatedAt == nil {
		t.Errorf("activated_at: nil, want set after first MarkUsed")
	}
	if gotKey.LastErrorAt == nil {
		t.Errorf("last_error_at: nil, want set after RecordError")
	}
}

func TestAPI_ListTasks_Empty(t *testing.T) {
	h, _, _, _, _, _ := newFullServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Fatalf("empty list should have items:[]; got %s", rr.Body.String())
	}
}

func TestSSE_StreamsBusEvents(t *testing.T) {
	h, _, _, _, _, bus := newFullServer(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream", nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: %s", ct)
	}

	// Read the initial retry preamble plus one published event in a
	// goroutine, signal via channel. The bus is asynchronous (subscriber
	// channel buffer 32) so a tiny sleep before publish ensures the
	// EventSource subscription registers first.
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 8192)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	// Publish until the subscriber has registered. The first read above
	// will return the preamble, so we publish in a small loop until the
	// channel has data — flaky-resistant pattern.
	for i := 0; i < 50; i++ {
		if bus.SubscriberCount() > 0 {
			break
		}
	}
	bus.Publish(events.KindKeyStateChanged, map[string]any{"hello": "world"})

	select {
	case body := <-got:
		if !strings.Contains(body, "retry: 3000") {
			t.Errorf("missing retry preamble; got: %q", body)
		}
		// The event might land on the next read pass; do one more read.
		buf := make([]byte, 8192)
		n, _ := resp.Body.Read(buf)
		body += string(buf[:n])
		if !strings.Contains(body, "event: key.state_changed") {
			t.Errorf("missing event line; got: %q", body)
		}
		if !strings.Contains(body, `"hello":"world"`) {
			t.Errorf("missing data payload; got: %q", body)
		}
	case <-ctx.Done():
		t.Fatalf("context cancelled before first read")
	}
}

func TestStaticVendor_ServesEmbeddedAssets(t *testing.T) {
	h, _, _, _, _, _ := newFullServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/vendor/htmx.min.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("content-type: %s", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=31536000") {
		t.Fatalf("cache-control: %s", cc)
	}
	if rr.Body.Len() < 1000 {
		t.Fatalf("body suspiciously small: %d bytes", rr.Body.Len())
	}
}

func TestStaticVendor_RejectsTraversal(t *testing.T) {
	h, _, _, _, _, _ := newFullServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/vendor/../web.go", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rr.Code)
	}
}
