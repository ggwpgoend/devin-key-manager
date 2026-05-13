package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

// JSON API layer. Mirrors the HTMX endpoints but returns JSON so external
// scripts (CLI, browser extensions, future UI components) can consume the
// same data without scraping HTML. Mounted under /api/v1.
//
// All list endpoints accept:
//
//	?limit=N            page size (default 50, max 500)
//	?cursor=<id>        keyset pagination: rows with id < cursor (DESC)
//	?sort=<field>       optional ordering field (whitelisted per endpoint)
//	?order=asc|desc     direction (default desc)
//	?filter=<key:val,…> comma-separated key:value filters
//
// The shape of every list response is {items: [...], next_cursor: "id"|""}.

func (s *Server) registerAPI(r chi.Router) {
	r.Route("/api/v1", func(r chi.Router) {
		// Read-only collection endpoints.
		r.Get("/keys", s.apiKeysList)
		r.Get("/keys/{id}", s.apiKeyGet)
		r.Get("/tasks", s.apiTasksList)
		r.Get("/tasks/{id}", s.apiTaskGet)
		r.Get("/sessions/{id}", s.apiSessionGet)
		r.Get("/sessions/{id}/messages", s.apiSessionMessages)
		r.Get("/sessions/{id}/artifacts", s.apiSessionArtifacts)
		r.Get("/artifacts/{id}", s.apiArtifactGet)

		// Health / metadata.
		r.Get("/health", s.apiHealth)
	})
}

// listParams captures the common query parameters parsed from a list request.
type listParams struct {
	Limit   int
	Cursor  string
	Sort    string
	Order   string // "asc" or "desc"
	Filters map[string]string
}

func parseListParams(r *http.Request) listParams {
	q := r.URL.Query()
	lp := listParams{
		Limit:   50,
		Cursor:  strings.TrimSpace(q.Get("cursor")),
		Sort:    strings.TrimSpace(q.Get("sort")),
		Order:   strings.ToLower(strings.TrimSpace(q.Get("order"))),
		Filters: map[string]string{},
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		if n > 500 {
			n = 500
		}
		lp.Limit = n
	}
	if lp.Order != "asc" && lp.Order != "desc" {
		lp.Order = "desc"
	}
	// filter=state:active,plan:trial → {state: active, plan: trial}
	if raw := q.Get("filter"); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			kv := strings.SplitN(pair, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.TrimSpace(kv[0])
			v := strings.TrimSpace(kv[1])
			if k != "" && v != "" {
				lp.Filters[k] = v
			}
		}
	}
	return lp
}

// --- /keys ---

type apiKey struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	Plan          string     `json:"plan"`
	State         string     `json:"state"`
	Fingerprint   string     `json:"fingerprint"`
	Notes         string     `json:"notes,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

func toAPIKey(k keys.Key) apiKey {
	return apiKey{
		ID:            k.ID,
		Label:         k.Label,
		Plan:          string(k.Plan),
		State:         string(k.State),
		Fingerprint:   k.Fingerprint,
		Notes:         k.Notes,
		CreatedAt:     k.CreatedAt,
		UpdatedAt:     k.UpdatedAt,
		LastUsedAt:    k.LastUsedAt,
		CooldownUntil: k.CooldownUntil,
	}
}

func (s *Server) apiKeysList(w http.ResponseWriter, r *http.Request) {
	lp := parseListParams(r)
	all, err := s.keys.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Apply filters in-memory: the keys table is small (rarely >100 rows)
	// so per-row predicate filtering is fine here. Once we hit thousands
	// of rows we'd push these into SQL WHERE clauses.
	out := make([]apiKey, 0, len(all))
	for _, k := range all {
		if v, ok := lp.Filters["state"]; ok && string(k.State) != v {
			continue
		}
		if v, ok := lp.Filters["plan"]; ok && string(k.Plan) != v {
			continue
		}
		out = append(out, toAPIKey(k))
	}
	// In-memory cursor: drop everything up to and including the cursor row.
	// IDs are opaque strings here; this is a simple "skip until" rather than
	// a sort-key cursor. Good enough for the foreseeable scale of /keys.
	if lp.Cursor != "" {
		skip := -1
		for i, k := range out {
			if k.ID == lp.Cursor {
				skip = i
				break
			}
		}
		if skip >= 0 {
			out = out[skip+1:]
		}
	}
	nextCursor := ""
	if len(out) > lp.Limit {
		nextCursor = out[lp.Limit-1].ID
		out = out[:lp.Limit]
	}
	writeJSON(w, listResponse[apiKey]{Items: out, NextCursor: nextCursor, Count: len(out)})
}

func (s *Server) apiKeyGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	k, err := s.keys.Get(r.Context(), id)
	if errors.Is(err, keys.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, toAPIKey(k))
}

// --- /tasks ---

type apiTask struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toAPITask(t tasks.Task) apiTask {
	return apiTask{
		ID:        t.ID,
		Title:     t.Title,
		Status:    string(t.Status),
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func (s *Server) apiTasksList(w http.ResponseWriter, r *http.Request) {
	lp := parseListParams(r)
	all, err := s.tasks.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiTask, 0, len(all))
	for _, t := range all {
		if v, ok := lp.Filters["status"]; ok && string(t.Status) != v {
			continue
		}
		out = append(out, toAPITask(t))
	}
	if lp.Cursor != "" {
		skip := -1
		for i, t := range out {
			if t.ID == lp.Cursor {
				skip = i
				break
			}
		}
		if skip >= 0 {
			out = out[skip+1:]
		}
	}
	nextCursor := ""
	if len(out) > lp.Limit {
		nextCursor = out[lp.Limit-1].ID
		out = out[:lp.Limit]
	}
	writeJSON(w, listResponse[apiTask]{Items: out, NextCursor: nextCursor, Count: len(out)})
}

func (s *Server) apiTaskGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := s.tasks.Get(r.Context(), id)
	if errors.Is(err, tasks.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Include the child sessions inline so callers don't have to do a
	// follow-up request for the chat list.
	sess, _ := s.sessions.ListByTask(r.Context(), id)
	apiSessions := make([]apiSession, 0, len(sess))
	for _, sx := range sess {
		apiSessions = append(apiSessions, toAPISession(sx))
	}
	writeJSON(w, struct {
		apiTask
		Sessions []apiSession `json:"sessions"`
	}{
		apiTask:  toAPITask(t),
		Sessions: apiSessions,
	})
}

// --- /sessions ---

type apiSession struct {
	ID             string     `json:"id"`
	TaskID         string     `json:"task_id"`
	KeyID          string     `json:"key_id"`
	DevinSessionID string     `json:"devin_session_id,omitempty"`
	Status         string     `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	EndReason      string     `json:"end_reason,omitempty"`
}

func toAPISession(s sessions.Session) apiSession {
	return apiSession{
		ID:             s.ID,
		TaskID:         s.TaskID,
		KeyID:          s.KeyID,
		DevinSessionID: s.DevinSessionID,
		Status:         string(s.Status),
		StartedAt:      s.StartedAt,
		EndedAt:        s.EndedAt,
		EndReason:      s.EndReason,
	}
}

func (s *Server) apiSessionGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessions.Get(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, toAPISession(sess))
}

type apiMessage struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

func (s *Server) apiSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.sessions.Get(r.Context(), id); errors.Is(err, sessions.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	msgs, err := s.sessions.ListMessages(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, apiMessage{
			ID:        m.ID,
			SessionID: m.SessionID,
			Role:      string(m.Role),
			Content:   m.Content,
			Timestamp: m.Timestamp,
		})
	}
	writeJSON(w, listResponse[apiMessage]{Items: out, Count: len(out)})
}

// --- /artifacts ---

type apiArtifact struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	SessionID   string    `json:"session_id"`
	Filename    string    `json:"filename,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	Status      string    `json:"status"`
	RemoteURL   string    `json:"remote_url,omitempty"`
	LocalURL    string    `json:"local_url,omitempty"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func toAPIArtifact(a artifacts.Artifact) apiArtifact {
	out := apiArtifact{
		ID:          a.ID,
		TaskID:      a.TaskID,
		SessionID:   a.SessionID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		Status:      string(a.Status),
		RemoteURL:   a.RemoteURL,
		Error:       a.Error,
		CreatedAt:   a.CreatedAt,
	}
	if a.Status == artifacts.StatusReady {
		out.LocalURL = "/artifacts/" + a.ID + "/raw"
	}
	return out
}

func (s *Server) apiSessionArtifacts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.artifacts == nil {
		writeJSON(w, listResponse[apiArtifact]{Items: []apiArtifact{}, Count: 0})
		return
	}
	rows, err := s.artifacts.ListBySession(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiArtifact, 0, len(rows))
	for _, a := range rows {
		out = append(out, toAPIArtifact(a))
	}
	writeJSON(w, listResponse[apiArtifact]{Items: out, Count: len(out)})
}

func (s *Server) apiArtifactGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.artifacts == nil {
		writeJSONError(w, http.StatusNotFound, "artifacts disabled")
		return
	}
	a, err := s.artifacts.Get(r.Context(), id)
	if errors.Is(err, artifacts.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, toAPIArtifact(a))
}

// --- /health ---

func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	subs := 0
	if s.bus != nil {
		subs = s.bus.SubscriberCount()
	}
	writeJSON(w, map[string]any{
		"status":          "ok",
		"sse_subscribers": subs,
		"time":            time.Now().UTC(),
	})
}

// --- helpers ---

type listResponse[T any] struct {
	Items      []T    `json:"items"`
	Count      int    `json:"count"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
