package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
)

// PR-17 / roadmap D40: artifact retention and pinning.
//
// Endpoints:
//   POST /api/artifacts/{id}/pin     — pin (default) or unpin via ?value=0
//   POST /api/artifacts/retention/sweep?max_age_hours=N[&dry_run=1]
//                                   — run a retention sweep, returning counts.
//
// Both endpoints are POST-only because they mutate state; the dry-run
// flag keeps "what would happen?" introspection cheap without separate
// GET variants.

func (s *Server) handleArtifactPin(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	pinned := true
	if r.URL.Query().Get("value") == "0" {
		pinned = false
	}
	if err := s.artifacts.SetPinned(r.Context(), id, pinned); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": id, "pinned": pinned})
}

func (s *Server) handleArtifactRetentionSweep(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	hoursStr := strings.TrimSpace(r.URL.Query().Get("max_age_hours"))
	if hoursStr == "" {
		hoursStr = "720" // 30 days default
	}
	hours, err := strconv.ParseInt(hoursStr, 10, 64)
	if err != nil || hours <= 0 {
		writeJSONError(w, http.StatusBadRequest, "max_age_hours must be > 0")
		return
	}
	opts := artifacts.PruneOptions{
		MaxAge: time.Duration(hours) * time.Hour,
		DryRun: r.URL.Query().Get("dry_run") == "1",
	}
	res, err := s.artifacts.Prune(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, res)
}
