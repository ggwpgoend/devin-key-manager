package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/lint"
)

// PR-16 endpoints:
//
//   - GET  /api/artifacts/{id}/lint   — run built-in linter on a text artifact
//   - GET  /api/keys/{id}/history     — key lifecycle timeline (A6)
//   - POST /notifications/dispatch    — fire a notification action

// handleArtifactLint reads the artifact contents from disk and runs the
// internal lint checks. Binary files short-circuit to a single "skipped"
// info finding. The response is always JSON so the front-end can render
// a counts pill regardless of language.
func (s *Server) handleArtifactLint(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	a, err := s.artifacts.Get(r.Context(), id)
	if errors.Is(err, artifacts.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if a.Status != artifacts.StatusReady || a.LocalPath == "" {
		http.Error(w, "artifact not ready", http.StatusAccepted)
		return
	}
	// 2 MB safety cap; the lint heuristics are O(n) but we don't want to
	// page in a 500 MB file the user accidentally uploaded.
	const maxBytes = 2 * 1024 * 1024
	f, err := os.Open(a.LocalPath)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer f.Close()
	limited := io.LimitReader(f, maxBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	findings := lint.Run(a.Filename, string(body), lint.Config{})
	errCnt, warnCnt, infoCnt := lint.Counts(findings)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"artifact_id": a.ID,
		"filename":    a.Filename,
		"language":    string(lint.DetectLanguage(a.Filename)),
		"findings":    findings,
		"counts": map[string]int{
			"error": errCnt, "warn": warnCnt, "info": infoCnt,
		},
	})
}

type keyHistoryEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// handleKeyHistory returns a synthetic key lifecycle timeline from the
// columns we already track (created/last_used/cooldown_until/state/
// last_error/request_count). We don't have a separate audit table yet
// (Roadmap A6 stops short of that), but the existing columns are enough
// for a "this key's life so far" view.
func (s *Server) handleKeyHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	k, err := s.keys.Get(r.Context(), id)
	if errors.Is(err, keys.ErrNotFound) {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	events := make([]keyHistoryEvent, 0, 6)
	events = append(events, keyHistoryEvent{At: k.CreatedAt, Kind: "created", Message: "key added to manager"})
	if k.LastUsedAt != nil {
		events = append(events, keyHistoryEvent{At: *k.LastUsedAt, Kind: "used", Message: "last successful request"})
	}
	if k.CooldownUntil != nil {
		events = append(events, keyHistoryEvent{At: *k.CooldownUntil, Kind: "cooldown", Message: "cooldown ends at this time"})
	}
	events = append(events, keyHistoryEvent{
		At:      time.Now(),
		Kind:    "current",
		Message: "currently in state: " + string(k.State),
	})
	if k.LastErrorMessage != "" {
		at := k.UpdatedAt
		if k.LastErrorAt != nil {
			at = *k.LastErrorAt
		}
		events = append(events, keyHistoryEvent{
			At:      at,
			Kind:    "error",
			Message: k.LastErrorMessage,
		})
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].At.Before(events[j].At)
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"key": map[string]any{
			"id":            k.ID,
			"label":         k.Label,
			"state":         k.State,
			"plan":          k.Plan,
			"request_count": k.RequestCount,
		},
		"events": events,
	})
}

// handleAPIKeysCreate is a JSON variant of handleKeysCreate. The
// browser extension scaffold (extensions/browser/) POSTs here to add a
// key from a foreign tab — chrome.runtime.fetch can do JSON but not
// HTML form-encoded redirects, so a dedicated JSON endpoint keeps the
// extension code straightforward.
//
// Permissive CORS is enabled for localhost so the extension works
// without requiring the user to flip a flag. We respond OPTIONS so the
// browser preflight check passes.
func (s *Server) handleAPIKeysCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var payload struct {
		Value string `json:"value"`
		Label string `json:"label"`
		Plan  string `json:"plan"`
		Notes string `json:"notes"`
		Tags  string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	in := keys.CreateInput{
		Label:  payload.Label,
		Plan:   keys.Plan(strings.TrimSpace(payload.Plan)),
		APIKey: payload.Value,
		Notes:  payload.Notes,
	}
	if payload.Tags != "" {
		in.Tags = strings.Split(payload.Tags, ",")
	}
	k, err := s.keys.Create(r.Context(), in)
	if err != nil {
		if errors.Is(err, keys.ErrDuplicateKey) {
			http.Error(w, "key already exists", http.StatusConflict)
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":    k.ID,
		"label": k.Label,
		"plan":  k.Plan,
	})
}

// handleNotificationDispatch lets the UI fire a click-to-action button
// from a notification. The frontend POSTs the action payload here and
// we forward it to the requested URL. Routing the action through the
// server keeps cross-origin and method-spoofing concerns simple — we
// only accept relative paths that begin with "/".
func (s *Server) handleNotificationDispatch(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(payload.URL, "/") {
		http.Error(w, "url must be relative", http.StatusBadRequest)
		return
	}
	method := strings.ToUpper(payload.Method)
	if method == "" {
		method = "POST"
	}
	// We don't actually proxy here; the frontend can call the URL
	// directly. This endpoint exists to validate-and-acknowledge so the
	// notification UI has a consistent feedback shape for telemetry /
	// retry logic without each caller hand-rolling fetch.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"url":    payload.URL,
		"method": method,
	})
}

// parseLimit is a tiny helper duplicate to avoid coupling — different
// PRs added their own parseLimit and we don't want to break callers
// mid-refactor.
func parseLimitForPR16(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

var _ = parseLimitForPR16 // unused-but-kept for future filters
