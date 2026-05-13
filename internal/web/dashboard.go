package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

// handleDashboard renders the storm dashboard at "/". KPI tiles are computed
// from current snapshots (keys.List, tasks.List, sessions.Stats); the page
// then patches the visible counters live via SSE without polling the server.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	allKeys, err := s.keys.List(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	allTasks, err := s.tasks.List(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sessStats, err := s.sessions.Stats(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	data := pageData{
		Title:         "Dashboard",
		Active:        "dashboard",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Stats:         computeStats(allKeys, allTasks, sessStats),
		RecentTasks:   recentTasks(allTasks, 6),
		KeyRollup:     keyRollupByPlan(allKeys),
	}
	s.renderPage(w, r, "dashboard", data)
}

func computeStats(allKeys []keys.Key, allTasks []tasks.Task, sessStats sessionStatsLike) dashStats {
	st := dashStats{
		KeysTotal:       len(allKeys),
		SessionsLast24h: sessStats.Last24h,
		SessionsOpen:    sessStats.Open,
		SessionsClosed:  sessStats.Closed,
	}
	for _, k := range allKeys {
		switch k.State {
		case keys.StateActive:
			st.KeysActive++
		case keys.StateCooldownDaily, keys.StateCooldownWeekly:
			st.KeysCooldown++
		case keys.StateDead:
			st.KeysDead++
		}
		st.RequestsTotal += k.RequestCount
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, t := range allTasks {
		if t.CreatedAt.After(cutoff) {
			st.TasksLast24h++
		}
		switch t.Status {
		case "running":
			st.TasksRunning++
		case "completed":
			st.TasksDone++
		}
	}
	return st
}

// sessionStatsLike is a small interface for tests / future stubbing. The
// concrete sessions.SessionStats struct satisfies it implicitly.
type sessionStatsLike = struct {
	Total       int
	Open        int
	Closed      int
	StartedLast time.Time
	Last24h     int
}

func recentTasks(all []tasks.Task, limit int) []tasks.Task {
	out := append([]tasks.Task(nil), all...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func keyRollupByPlan(all []keys.Key) []keyRollup {
	type bucket struct {
		count int
		reqs  int64
	}
	by := map[keys.Plan]*bucket{}
	for _, k := range all {
		b, ok := by[k.Plan]
		if !ok {
			b = &bucket{}
			by[k.Plan] = b
		}
		b.count++
		b.reqs += k.RequestCount
	}
	out := make([]keyRollup, 0, len(by))
	for p, b := range by {
		out = append(out, keyRollup{
			Plan:      string(p),
			Count:     b.count,
			Requests:  b.reqs,
			PillClass: planPill(p),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func planPill(p keys.Plan) string {
	switch p {
	case keys.PlanFree:
		return "ok"
	case keys.PlanTrial:
		return "wait"
	default:
		return "run"
	}
}
