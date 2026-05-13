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
	"sync/atomic"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
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

// schedulesHarness wires the same minimal in-memory stack used by the
// artifacts e2e test but with notifications + schedules attached. The
// devinSrv argument lets the caller stub out responses for the Devin API
// (specifically `POST /sessions`) so the test can verify that a "Run now"
// click on a schedule actually opens a session via the Manager.
type schedulesHarness struct {
	dir     string
	handler http.Handler
	manager *manager.Manager
	repo    *schedules.Repo
	notifs  *notifications.Repo
	cleanup func()
}

func newSchedulesHarness(t *testing.T, devinSrv *httptest.Server) *schedulesHarness {
	t.Helper()
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
	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	artRepo := artifacts.NewRepo(db)
	schedRepo := schedules.NewRepo(db)
	notifRepo := notifications.NewRepo(db)

	factory := func(string) *devin.Client {
		return devin.NewClient("sk-mock", devin.WithBaseURL(devinSrv.URL))
	}
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientFactory: factory,
		Artifacts:     artRepo,
		Notifications: notifRepo,
	})
	if _, err := keysRepo.Create(ctx, keys.CreateInput{
		Label: "trial-sched", Plan: keys.PlanTrial, APIKey: "sk-mock-sched",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	srv, err := web.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)),
		web.Deps{
			Keys:          keysRepo,
			Tasks:         tasksRepo,
			Sessions:      sessionsRepo,
			Handoffs:      handoffsRepo,
			Artifacts:     artRepo,
			Schedules:     schedRepo,
			Notifications: notifRepo,
			Manager:       mgr,
		},
		filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("web server: %v", err)
	}

	return &schedulesHarness{
		dir:     dir,
		handler: srv.Handler(),
		manager: mgr,
		repo:    schedRepo,
		notifs:  notifRepo,
		cleanup: func() { _ = db.Close() },
	}
}

func TestSchedulesIndex_RendersNav(t *testing.T) {
	devinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-mock-sched","url":"https://app.devin.ai/sessions/devin-mock-sched"}`)
	}))
	t.Cleanup(devinSrv.Close)
	h := newSchedulesHarness(t, devinSrv)
	t.Cleanup(h.cleanup)

	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/schedules", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /schedules: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Scheduled tasks", "New schedule", "Daily at", `name="kind" value="interval"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Nav active state shows Schedules with text-white class.
	if !strings.Contains(body, `href="/schedules" class="text-white"`) {
		t.Errorf("nav active state missing for schedules")
	}
}

func TestSchedules_CreateListToggleDelete(t *testing.T) {
	devinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"devin-mock-sched","url":"https://app.devin.ai/sessions/devin-mock-sched"}`)
	}))
	t.Cleanup(devinSrv.Close)
	h := newSchedulesHarness(t, devinSrv)
	t.Cleanup(h.cleanup)

	form := url.Values{
		"title":            {"Hourly check"},
		"prompt":           {"Run a quick health check."},
		"kind":             {"interval"},
		"interval_seconds": {"3600"},
		"daily_hour":       {"9"},
		"daily_minute":     {"0"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/schedules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /schedules: %d body: %s", rr.Code, rr.Body.String())
	}

	all, err := h.repo.List(context.Background())
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(all))
	}
	sch := all[0]
	if !sch.Enabled || sch.IntervalSeconds != 3600 {
		t.Errorf("unexpected schedule state: %+v", sch)
	}

	// GET /schedules now renders the row.
	rr = httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/schedules", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Hourly check") {
		t.Errorf("schedules page missing new schedule title; body=%s", body)
	}

	// Toggle disables it.
	rr = httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/schedules/"+sch.ID+"/toggle", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("toggle: %d body: %s", rr.Code, rr.Body.String())
	}
	after, _ := h.repo.Get(context.Background(), sch.ID)
	if after.Enabled {
		t.Errorf("toggle should have disabled the schedule")
	}

	// Delete via DELETE.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/schedules/"+sch.ID, nil)
	req.Header.Set("HX-Request", "true")
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d body: %s", rr.Code, rr.Body.String())
	}
	if _, err := h.repo.Get(context.Background(), sch.ID); err == nil {
		t.Errorf("schedule still exists after delete")
	}
}

func TestSchedules_RunNow_FiresTaskAndAppendsEvent(t *testing.T) {
	var devinHits atomic.Int64
	devinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions") && r.Method == http.MethodPost {
			devinHits.Add(1)
			_, _ = io.WriteString(w, `{"session_id":"devin-mock-sched","url":"https://app.devin.ai/sessions/devin-mock-sched"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(devinSrv.Close)
	h := newSchedulesHarness(t, devinSrv)
	t.Cleanup(h.cleanup)

	// Seed schedule via the form.
	form := url.Values{
		"title":            {"Run me now"},
		"prompt":           {"echo hi"},
		"kind":             {"interval"},
		"interval_seconds": {"3600"},
		"daily_hour":       {"0"},
		"daily_minute":     {"0"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/schedules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d body: %s", rr.Code, rr.Body.String())
	}
	all, _ := h.repo.List(context.Background())
	if len(all) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(all))
	}
	sch := all[0]

	// POST /schedules/{id}/run -> async goroutine -> Manager.StartScheduledTask -> Devin mock fires.
	rr = httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/schedules/"+sch.ID+"/run", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("run: %d body: %s", rr.Code, rr.Body.String())
	}

	// The Devin call happens off-request in a goroutine; wait for it to
	// post (and the resulting MarkRan + Append to complete) before
	// asserting state. 2 seconds is plenty for an in-process httptest
	// mock; never reached in the happy path.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if devinHits.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if devinHits.Load() == 0 {
		t.Errorf("expected Devin /sessions POST, got 0 hits")
	}

	// Notification event must be present (poll for it — also async).
	var evs []notifications.Event
	for time.Now().Before(deadline) {
		var rErr error
		evs, rErr = h.notifs.Recent(context.Background(), 10)
		if rErr != nil {
			t.Fatalf("recent: %v", rErr)
		}
		if len(evs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(evs) == 0 {
		t.Fatalf("no events emitted for manual run")
	}
	sawSchedFired := false
	for _, e := range evs {
		if e.Kind == notifications.KindScheduleFired {
			sawSchedFired = true
			if !strings.Contains(e.Title, "Run me now") {
				t.Errorf("event title=%q does not mention schedule title", e.Title)
			}
		}
	}
	if !sawSchedFired {
		t.Errorf("missing schedule_fired event in %v", evs)
	}

	// last_run_at should be set on the schedule row.
	var afterRun schedules.Schedule
	for time.Now().Before(deadline) {
		afterRun, _ = h.repo.Get(context.Background(), sch.ID)
		if afterRun.LastRunAt.Valid && afterRun.LastSessionID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !afterRun.LastRunAt.Valid {
		t.Errorf("last_run_at not set after manual run")
	}
	if afterRun.LastSessionID == "" {
		t.Errorf("last_session_id not set after manual run")
	}
}

func TestEventsSince_HighWaterMark(t *testing.T) {
	devinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(devinSrv.Close)
	h := newSchedulesHarness(t, devinSrv)
	t.Cleanup(h.cleanup)

	ctx := context.Background()
	id1, _ := h.notifs.Append(ctx, notifications.AppendInput{Title: "first", Kind: notifications.KindSystem})
	id2, _ := h.notifs.Append(ctx, notifications.AppendInput{Title: "second", Kind: notifications.KindSystem})

	// Empty after-cursor → both events.
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events/since", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("events: %d body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Events []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"events"`
		LastID int64 `json:"last_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 2 || resp.Events[0].ID != id1 || resp.Events[1].ID != id2 {
		t.Errorf("unexpected events: %+v", resp.Events)
	}
	if resp.LastID != id2 {
		t.Errorf("last_id: want %d, got %d", id2, resp.LastID)
	}

	// After id1 → only second event.
	rr = httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events/since?after="+itoa(id1), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].ID != id2 {
		t.Errorf("after-cursor wrong: %+v", resp.Events)
	}

	// Sanity: scheduler fires also produce events. Mark a synthetic event
	// and assert it lands.
	if _, err := h.notifs.Append(ctx, notifications.AppendInput{
		Kind: notifications.KindDevinMessage, Title: "Devin replied", URL: "/sessions/x",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Sleep a hair so timestamps are deterministic in case of identical ms.
	time.Sleep(5 * time.Millisecond)
	rr = httptest.NewRecorder()
	h.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events/since?after="+itoa(id2), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode3: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Title != "Devin replied" {
		t.Errorf("after id2 wrong: %+v", resp.Events)
	}
}

// itoa avoids importing strconv at the test top just for this one helper.
func itoa(i int64) string {
	return formatInt64(i)
}

func formatInt64(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
