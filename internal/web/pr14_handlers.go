package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/observability"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

// PR-14: observability tab.
//
// Architecture: handleObservabilityIndex renders a stand-alone HTML page
// that the user lands on at /observability. The page bootstraps a small
// amount of state (the task list, so the user can pick which task to
// session-graph) and then fetches all chart data on-demand via the JSON
// endpoints under /api/observability. This keeps the HTML rendering side
// dumb and the API independently usable from the CLI / extension.

func (s *Server) handleObservabilityIndex(w http.ResponseWriter, r *http.Request) {
	allTasks, err := s.tasks.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPage(w, r, "observability", pageData{
		Title:         "Observability",
		Active:        "observability",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		TasksAll:      allTasks,
	})
}

// /api/observability/timeseries?metric=<name>&bucket=hour|day&window_hours=<n>
//
// metric is one of: sessions, messages, handoffs, tasks.
func (s *Server) handleObservabilityTimeseries(w http.ResponseWriter, r *http.Request) {
	if s.observability == nil {
		http.Error(w, "observability disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	metric := strings.ToLower(strings.TrimSpace(q.Get("metric")))
	bucket := observability.BucketHour
	if strings.ToLower(q.Get("bucket")) == "day" {
		bucket = observability.BucketDay
	}
	window := parseHoursWindow(q.Get("window_hours"), 24*time.Hour)

	var (
		series observability.Series
		err    error
	)
	switch metric {
	case "sessions":
		series, err = s.observability.SessionsStarted(r.Context(), bucket, window)
	case "messages":
		series, err = s.observability.MessagesSent(r.Context(), bucket, window)
	case "handoffs":
		series, err = s.observability.HandoffsCreated(r.Context(), bucket, window)
	case "tasks":
		series, err = s.observability.TasksCreated(r.Context(), bucket, window)
	default:
		http.Error(w, "unknown metric (want: sessions|messages|handoffs|tasks)", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(series)
}

func (s *Server) handleObservabilityStateBreakdown(w http.ResponseWriter, r *http.Request) {
	if s.observability == nil {
		http.Error(w, "observability disabled", http.StatusServiceUnavailable)
		return
	}
	window := parseHoursWindow(r.URL.Query().Get("window_hours"), 24*time.Hour)
	bd, err := s.observability.SessionStateBreakdown(r.Context(), window)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"window_hours": int(window / time.Hour),
		"counts":       bd,
	})
}

func (s *Server) handleObservabilityTopKeys(w http.ResponseWriter, r *http.Request) {
	if s.observability == nil {
		http.Error(w, "observability disabled", http.StatusServiceUnavailable)
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 10)
	rows, err := s.observability.KeyUsageTop(r.Context(), limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": rows})
}

func (s *Server) handleObservabilitySessionGraph(w http.ResponseWriter, r *http.Request) {
	if s.observability == nil {
		http.Error(w, "observability disabled", http.StatusServiceUnavailable)
		return
	}
	taskID := chi.URLParam(r, "task_id")
	g, err := s.observability.SessionGraphForTask(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g)
}

// parseHoursWindow accepts "24", "168", etc., capping at 4 weeks. Falls
// back to def on any parse error or out-of-range value.
func parseHoursWindow(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	v := parseLimit(raw, int(def/time.Hour))
	if v <= 0 {
		return def
	}
	max := 24 * 7 * 4 // four weeks
	if v > max {
		v = max
	}
	return time.Duration(v) * time.Hour
}
