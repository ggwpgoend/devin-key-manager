package web

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// PR-18 / roadmap B20 + D34:
//
//   POST /api/sessions/{id}/diagnose   — pure diagnosis, no side effects.
//   POST /api/sessions/{id}/resume     — diagnose + act (continue / handoff).
//   GET  /api/sessions/{id}/versions   — list ready-artifact filename groups
//                                        that have more than one version.
//   GET  /api/artifacts/diff?left=ID&right=ID — unified diff between two
//                                        ready artifacts.
//
// Diagnose and Resume are HTTP-level wrappers around the corresponding
// manager methods. The diff endpoint is GET because it has no side
// effects and is safe to refresh.

func (s *Server) handleSessionDiagnose(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil {
		http.Error(w, "manager not configured", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	d, err := s.manager.DiagnoseSession(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"session_id":    d.SessionID,
		"reason":        string(d.Reason),
		"action":        string(d.Action),
		"detail":        d.Detail,
		"messages_seen": d.MessagesSeen,
		"idle_for_s":    int(d.IdleFor.Seconds()),
	})
}

func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil {
		http.Error(w, "manager not configured", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	d, err := s.manager.ResumeSession(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"session_id":    d.SessionID,
		"reason":        string(d.Reason),
		"action":        string(d.Action),
		"detail":        d.Detail,
		"messages_seen": d.MessagesSeen,
		"idle_for_s":    int(d.IdleFor.Seconds()),
	})
}

func (s *Server) handleSessionArtifactVersions(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	groups, err := s.artifacts.VersionsByName(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
		SizeBytes int64  `json:"size_bytes"`
	}
	type group struct {
		Filename string `json:"filename"`
		Versions []row  `json:"versions"`
	}
	out := make([]group, 0, len(groups))
	for _, g := range groups {
		rows := make([]row, 0, len(g.Artifacts))
		for _, a := range g.Artifacts {
			rows = append(rows, row{
				ID:        a.ID,
				CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
				SizeBytes: a.SizeBytes,
			})
		}
		out = append(out, group{Filename: g.Filename, Versions: rows})
	}
	writeJSON(w, out)
}

func (s *Server) handleArtifactDiff(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	left := strings.TrimSpace(r.URL.Query().Get("left"))
	right := strings.TrimSpace(r.URL.Query().Get("right"))
	if left == "" || right == "" {
		writeJSONError(w, http.StatusBadRequest, "left and right query params required")
		return
	}
	body, err := s.artifacts.Diff(r.Context(), left, right)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}
