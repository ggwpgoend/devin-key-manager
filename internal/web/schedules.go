package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

// handleSchedulesIndex renders the schedules dashboard with the creation
// form and the list of existing schedules.
func (s *Server) handleSchedulesIndex(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		http.Error(w, "schedules disabled", http.StatusNotFound)
		return
	}
	list, err := s.schedules.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	tz, _ := now.Zone()
	s.renderPage(w, r, "schedules_index", pageData{
		Title:         "Schedules",
		Active:        "schedules",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Schedules:     list,
		ServerTime:    now.Format("15:04"),
		ServerTZ:      tz,
		ScheduleForm: scheduleFormData{
			Kind:            string(schedules.KindInterval),
			IntervalSeconds: 3600,
			DailyHour:       9,
			DailyMinute:     0,
		},
	})
}

// handleSchedulesCreate validates the form and inserts a new schedule.
func (s *Server) handleSchedulesCreate(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		http.Error(w, "schedules disabled", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	form := scheduleFormData{
		Title:    strings.TrimSpace(r.FormValue("title")),
		Prompt:   strings.TrimSpace(r.FormValue("prompt")),
		PlanHint: strings.TrimSpace(r.FormValue("plan_hint")),
		Kind:     strings.TrimSpace(r.FormValue("kind")),
	}
	form.IntervalSeconds = parseInt64(r.FormValue("interval_seconds"), 0)
	form.DailyHour = int(parseInt64(r.FormValue("daily_hour"), 0))
	form.DailyMinute = int(parseInt64(r.FormValue("daily_minute"), 0))

	in := schedules.CreateInput{
		Title:           form.Title,
		Prompt:          form.Prompt,
		PlanHint:        form.PlanHint,
		Kind:            schedules.Kind(form.Kind),
		IntervalSeconds: form.IntervalSeconds,
		DailyHour:       form.DailyHour,
		DailyMinute:     form.DailyMinute,
		Enabled:         true,
	}
	if _, err := s.schedules.Create(r.Context(), in); err != nil {
		list, _ := s.schedules.List(r.Context())
		now := time.Now()
		tz, _ := now.Zone()
		s.renderPage(w, r, "schedules_index", pageData{
			Title:         "Schedules",
			Active:        "schedules",
			Version:       version.Version,
			MasterKeyPath: s.masterKeyPath,
			Schedules:     list,
			ServerTime:    now.Format("15:04"),
			ServerTZ:      tz,
			ScheduleForm:  form,
			ScheduleError: err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/schedules", http.StatusSeeOther)
}

// handleSchedulesToggle flips the enabled flag.
func (s *Server) handleSchedulesToggle(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		http.Error(w, "schedules disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	sch, err := s.schedules.Get(r.Context(), id)
	if errors.Is(err, schedules.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.schedules.SetEnabled(r.Context(), id, !sch.Enabled); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/schedules", http.StatusSeeOther)
}

// handleSchedulesRunNow fires a schedule immediately. The actual Devin
// call happens in a detached goroutine so the user's HTTP request returns
// instantly — closing the browser tab doesn't abort the in-flight create.
// The schedule row is marked-ran (and a notification appended) once the
// background work completes; the user observes results through the
// /schedules list and the notification bell.
func (s *Server) handleSchedulesRunNow(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil || s.manager == nil {
		http.Error(w, "schedules disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	sch, err := s.schedules.Get(r.Context(), id)
	if errors.Is(err, schedules.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	go s.runScheduleAsync(sch, true)
	http.Redirect(w, r, "/schedules?flash="+urlEncode("Run queued — check the bell in a moment."), http.StatusSeeOther)
}

// runScheduleAsync executes a schedule with its own background context so
// it survives the originating HTTP request (or scheduler tick) cancelling.
// Used by both manual run-now and the scheduler tick to avoid coupling
// long-running Devin calls to the caller's context.
func (s *Server) runScheduleAsync(sch schedules.Schedule, manual bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sid, runErr := s.manager.StartScheduledTask(ctx, sch)
	outcome := schedules.RunOutcome{SessionID: sid}
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	if err := s.schedules.MarkRan(ctx, sch.ID, outcome); err != nil {
		s.logger.Warn("schedules: mark ran", "id", sch.ID, "err", err)
	}
	if s.notifs != nil {
		suffix := ""
		if manual {
			suffix = " (manual run)"
		}
		title := "Scheduled: " + sch.Title + suffix
		body := sch.Prompt
		urlStr := ""
		if sid != "" {
			urlStr = "/sessions/" + sid
		}
		if runErr != nil {
			body = runErr.Error()
		}
		if _, err := s.notifs.Append(ctx, notifications.AppendInput{
			Kind:             notifications.KindScheduleFired,
			Title:            title,
			Body:             body,
			URL:              urlStr,
			RelatedSessionID: sid,
		}); err != nil {
			s.logger.Warn("schedules: notify", "id", sch.ID, "err", err)
		}
	}
	if runErr != nil {
		s.logger.Warn("schedules: run failed", "id", sch.ID, "manual", manual, "err", runErr)
	}
}

// handleSchedulesDelete removes a schedule.
func (s *Server) handleSchedulesDelete(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		http.Error(w, "schedules disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.schedules.Delete(r.Context(), id); err != nil && !errors.Is(err, schedules.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/schedules", http.StatusSeeOther)
}

// handleEventsSince streams notification events with id > ?after=<n>. Used by
// the layout's browser-notification JS to poll for new toasts.
func (s *Server) handleEventsSince(w http.ResponseWriter, r *http.Request) {
	if s.notifs == nil {
		writeJSON(w, eventsResponse{Events: []eventJSON{}, LastID: 0})
		return
	}
	after := parseInt64(r.URL.Query().Get("after"), 0)
	limit := int(parseInt64(r.URL.Query().Get("limit"), 50))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	evs, err := s.notifs.Since(ctx, after, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	out := make([]eventJSON, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventJSON{
			ID:        e.ID,
			Kind:      string(e.Kind),
			Title:     e.Title,
			Body:      e.Body,
			URL:       e.URL,
			TaskID:    e.RelatedTaskID,
			SessionID: e.RelatedSessionID,
			CreatedAt: e.CreatedAt.UnixMilli(),
		})
	}
	// LastID is the highest event id at query time. The frontend uses it
	// to seed its cursor on the first empty poll so a freshly-opened page
	// doesn't toast the entire history. Returning a millisecond timestamp
	// here (as a previous revision did) is a bug — the cursor compares to
	// event ids, which are small auto-increment integers.
	var lastID int64
	if len(evs) > 0 {
		lastID = evs[len(evs)-1].ID
	} else if m, mErr := s.notifs.MaxID(ctx); mErr == nil {
		lastID = m
	}
	writeJSON(w, eventsResponse{Events: out, LastID: lastID})
}

type eventsResponse struct {
	Events []eventJSON `json:"events"`
	LastID int64       `json:"last_id"`
}

type eventJSON struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	TaskID    string `json:"task_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Fallback — JSON marshalling of our own structs really shouldn't fail.
		fmt.Fprintf(w, `{"error":%q}`, err.Error())
	}
}

func parseInt64(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
