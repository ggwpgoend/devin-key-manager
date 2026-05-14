package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/attachments"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

// --- A3 / A4: keys tags + bulk delete ---

// handleKeysSetTags overwrites the tag list on a single key. The form
// payload is "tags=tag1,tag2,..." or "tags=tag1&tags=tag2&..."; both are
// accepted.
func (s *Server) handleKeysSetTags(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := r.Form["tags"]
	// Also accept a single comma-joined "tags" param for convenience.
	if len(raw) == 1 && strings.Contains(raw[0], ",") {
		raw = strings.Split(raw[0], ",")
	}
	if err := s.keys.SetTags(r.Context(), id, raw); err != nil {
		if errors.Is(err, keys.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// HTMX clients get a JSON ack; browsers redirect back to the keys list.
	if isHTMX(r) || acceptsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

// handleKeysBulkDelete removes many keys at once. POST with one or more
// "id=..." parameters. If ?dry_run=1 is set, returns a preview JSON of
// which IDs would be deleted (and how many are active vs cooldown) WITHOUT
// committing.
func (s *Server) handleKeysBulkDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids := r.Form["id"]
	// Also accept "ids=a,b,c" for JSON-style callers.
	if v := strings.TrimSpace(r.FormValue("ids")); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				ids = append(ids, p)
			}
		}
	}
	if len(ids) == 0 {
		http.Error(w, "no ids provided", http.StatusBadRequest)
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "1" || r.FormValue("dry_run") == "1"

	// Load each id so we can return a useful preview / refuse missing ones.
	type previewRow struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Plan  string `json:"plan"`
		State string `json:"state"`
	}
	preview := make([]previewRow, 0, len(ids))
	stateCount := map[string]int{}
	for _, id := range ids {
		k, err := s.keys.Get(r.Context(), id)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		} else if err != nil {
			s.serverError(w, r, err)
			return
		}
		preview = append(preview, previewRow{ID: k.ID, Label: k.Label, Plan: string(k.Plan), State: string(k.State)})
		stateCount[string(k.State)]++
	}

	if dryRun {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":  len(preview),
			"states": stateCount,
			"keys":   preview,
		})
		return
	}

	deleteIDs := make([]string, 0, len(preview))
	for _, p := range preview {
		deleteIDs = append(deleteIDs, p.ID)
	}
	deleted, err := s.keys.DeleteMany(r.Context(), deleteIDs)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if acceptsJSON(r) || isHTMX(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": deleted})
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/keys?flash=Deleted+%d+keys", deleted), http.StatusSeeOther)
}

// --- C22: pin/unpin messages ---

func (s *Server) handleMessagePin(w http.ResponseWriter, r *http.Request) {
	s.setMessagePin(w, r, true)
}
func (s *Server) handleMessageUnpin(w http.ResponseWriter, r *http.Request) {
	s.setMessagePin(w, r, false)
}
func (s *Server) setMessagePin(w http.ResponseWriter, r *http.Request, pinned bool) {
	id := chi.URLParam(r, "id")
	if err := s.sessions.SetPinned(r.Context(), id, pinned); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if acceptsJSON(r) || isHTMX(r) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pinned":%t}`, pinned)
		return
	}
	http.Redirect(w, r, r.Header.Get("Referer"), http.StatusSeeOther)
}

// --- C29: chat search within one session ---

func (s *Server) handleSessionSearch(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	q := r.URL.Query().Get("q")
	limitN := parseLimit(r.URL.Query().Get("limit"), 50)
	hits, err := s.sessions.SearchMessages(r.Context(), q, sessionID, limitN)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeSearchResponse(w, r, hits, q)
}

// handleGlobalSearch is the K81-flavour: FTS5 across every session.
func (s *Server) handleGlobalSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limitN := parseLimit(r.URL.Query().Get("limit"), 100)
	hits, err := s.sessions.SearchMessages(r.Context(), q, "", limitN)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeSearchResponse(w, r, hits, q)
}

func writeSearchResponse(w http.ResponseWriter, r *http.Request, hits []sessions.SearchHit, q string) {
	if acceptsJSON(r) || isHTMX(r) {
		type respHit struct {
			MessageID string `json:"message_id"`
			SessionID string `json:"session_id"`
			TaskID    string `json:"task_id"`
			TaskTitle string `json:"task_title"`
			Role      string `json:"role"`
			Snippet   string `json:"snippet"`
			Ts        string `json:"ts"`
		}
		out := make([]respHit, 0, len(hits))
		for _, h := range hits {
			out = append(out, respHit{
				MessageID: h.Message.ID,
				SessionID: h.Message.SessionID,
				TaskID:    h.TaskID,
				TaskTitle: h.TaskTitle,
				Role:      string(h.Message.Role),
				Snippet:   h.Snippet,
				Ts:        h.Message.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"query": q, "hits": out})
		return
	}
	// Fallback HTML: minimal list. The user-facing JS calls this with HTMX
	// to render results inline, so this branch is rarely exercised.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<div class="s-card"><h3>Search results for %q</h3><ol>`, q)
	for _, h := range hits {
		_, _ = fmt.Fprintf(w, `<li><a href="/sessions/%s">%s</a> — %s</li>`, h.Message.SessionID, h.TaskTitle, h.Snippet)
	}
	_, _ = io.WriteString(w, `</ol></div>`)
}

// --- C30: continue from stopped ---

// handleSessionContinue sends a "continue" prompt to a session that appears
// to have stopped mid-output. Caller is responsible for deciding when to
// invoke this; the UI shows the button next to sessions whose last
// message looks truncated (heuristic: ends with "..." or hits a token
// boundary).
func (s *Server) handleSessionContinue(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.manager == nil {
		http.Error(w, "manager not available", http.StatusServiceUnavailable)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		prompt = "continue"
	}
	if err := s.manager.SendFollowUp(r.Context(), id, prompt); err != nil {
		s.serverError(w, r, err)
		return
	}
	if acceptsJSON(r) || isHTMX(r) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

// --- C26: file upload via file.io ---

// handleSessionAttach accepts a multipart POST with a single "file" form
// field, uploads it to the configured provider, then sends a follow-up
// message containing the public URL so Devin can fetch the bytes.
//
// Size cap is 25 MB by default — file.io free tier accepts up to 100 MB
// but 25 MB keeps the manager process from being a memory hog on large
// pastes. Configurable via env DEVINMGR_ATTACH_MAX_BYTES (not wired in
// PR-12; default is hard-coded for simplicity).
const maxAttachmentBytes = 25 * 1024 * 1024

func (s *Server) handleSessionAttach(w http.ResponseWriter, r *http.Request) {
	if s.attachments == nil {
		http.Error(w, "attachments disabled", http.StatusServiceUnavailable)
		return
	}
	sessionID := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+1024)
	if err := r.ParseMultipartForm(maxAttachmentBytes); err != nil {
		http.Error(w, "file too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
	if err != nil {
		http.Error(w, "read file: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxAttachmentBytes {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	mime := header.Header.Get("Content-Type")
	att, err := s.attachments.Create(r.Context(), sessionID, header.Filename, mime, body)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Auto-relay: if the user wants the URL sent to Devin immediately,
	// they can pass relay=1 in the form. Default is yes since that's the
	// whole point of C26.
	if r.FormValue("relay") != "0" && s.manager != nil {
		note := r.FormValue("note")
		prompt := buildAttachmentPrompt(att, note)
		if err := s.manager.SendFollowUp(r.Context(), sessionID, prompt); err != nil {
			s.logger.Warn("relay attachment", "err", err)
		}
	}

	if acceptsJSON(r) || isHTMX(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         att.ID,
			"filename":   att.Filename,
			"public_url": att.PublicURL,
			"size_bytes": att.SizeBytes,
			"status":     string(att.Status),
		})
		return
	}
	http.Redirect(w, r, "/sessions/"+sessionID, http.StatusSeeOther)
}

func buildAttachmentPrompt(a attachments.Attachment, note string) string {
	var b strings.Builder
	b.WriteString("I uploaded a file for you: ")
	b.WriteString(a.Filename)
	b.WriteString(" (")
	b.WriteString(humanBytes(a.SizeBytes))
	b.WriteString(", ")
	if a.MimeType != "" {
		b.WriteString(a.MimeType)
	} else {
		b.WriteString("binary")
	}
	b.WriteString("). Fetch it from: ")
	b.WriteString(a.PublicURL)
	if note = strings.TrimSpace(note); note != "" {
		b.WriteString("\n\nNote: ")
		b.WriteString(note)
	}
	return b.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (s *Server) handleSessionAttachmentsList(w http.ResponseWriter, r *http.Request) {
	if s.attachments == nil {
		http.Error(w, "attachments disabled", http.StatusServiceUnavailable)
		return
	}
	sessionID := chi.URLParam(r, "id")
	list, err := s.attachments.ListBySession(r.Context(), sessionID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"attachments": list})
}

// --- B14: fork/checkpoint ---

// handleSessionFork creates a B14 checkpoint fork. Form params:
//   - anchor_id: optional message id to fork at (inclusive). Omit to fork
//     at the very end (clone entire history).
//   - task_id: optional task to assign the fork to. Defaults to source's.
//   - key_id: optional key to bind to. Defaults to source's.
//
// The fork is local-only; the new session is in "creating" state and will
// pick up a real DevinSessionID on its first user message via the manager.
func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in := sessions.ForkInput{
		SourceID:        id,
		AnchorMessageID: strings.TrimSpace(r.FormValue("anchor_id")),
		TaskID:          strings.TrimSpace(r.FormValue("task_id")),
		KeyID:           strings.TrimSpace(r.FormValue("key_id")),
	}
	forked, err := s.sessions.Fork(r.Context(), in)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if acceptsJSON(r) || isHTMX(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                     forked.ID,
			"task_id":                forked.TaskID,
			"forked_from_session_id": forked.ForkedFromSessionID,
			"forked_from_message_id": forked.ForkedFromMessageID,
		})
		return
	}
	http.Redirect(w, r, "/sessions/"+forked.ID, http.StatusSeeOther)
}

// --- helpers shared with the JSON-API ---

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func acceptsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func parseLimit(raw string, def int) int {
	if raw == "" {
		return def
	}
	var n int
	_, err := fmt.Sscanf(raw, "%d", &n)
	if err != nil || n <= 0 || n > 500 {
		return def
	}
	return n
}
