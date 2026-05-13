// Package web wires the HTTP routes for the local dashboard. Templates and
// static assets are embedded so the binary remains a single file.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server is the HTTP layer of the dashboard.
type Server struct {
	logger        *slog.Logger
	keys          *keys.Repo
	tasks         *tasks.Repo
	sessions      *sessions.Repo
	handoffs      *handoffs.Repo
	artifacts     *artifacts.Repo
	manager       *manager.Manager
	pages         map[string]*template.Template
	partials      *template.Template
	masterKeyPath string
}

// pageContentFiles enumerates the content templates that render as full pages.
// Each entry is parsed alongside layout.html into its own *template.Template so
// that multiple pages can each define a {{define "content"}} block without
// clashing in a single shared parse tree.
var pageContentFiles = map[string]string{
	"keys_index":    "templates/keys_index.html",
	"tasks_index":   "templates/tasks_index.html",
	"task_detail":   "templates/task_detail.html",
	"session_chat":  "templates/session_chat.html",
	"session_files": "templates/session_files.html",
}

// partialFiles enumerates the HTMX partial templates rendered without the
// layout chrome (dialogs, table fragments, etc.). They share a single parse
// tree because their {{define ...}} names do not collide.
var partialFiles = []string{
	"templates/keys_index.html",
	"templates/keys_form.html",
	"templates/task_form.html",
	"templates/session_chat.html",
}

// Deps bundles the repositories and orchestrator the web layer depends on.
type Deps struct {
	Keys      *keys.Repo
	Tasks     *tasks.Repo
	Sessions  *sessions.Repo
	Handoffs  *handoffs.Repo
	Artifacts *artifacts.Repo
	Manager   *manager.Manager
}

// NewServer compiles templates and prepares the handler. masterKeyPath is
// shown in the footer so users always know where their encryption key lives.
func NewServer(logger *slog.Logger, deps Deps, masterKeyPath string) (*Server, error) {
	layoutBody, err := templatesFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: read layout: %w", err)
	}

	funcs := template.FuncMap{"now": time.Now}

	pages := make(map[string]*template.Template, len(pageContentFiles))
	for name, path := range pageContentFiles {
		contentBody, err := templatesFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("web: read %s: %w", path, err)
		}
		tpl := template.New(name).Funcs(funcs)
		if _, err := tpl.Parse(string(layoutBody)); err != nil {
			return nil, fmt.Errorf("web: parse layout for %s: %w", name, err)
		}
		if _, err := tpl.Parse(string(contentBody)); err != nil {
			return nil, fmt.Errorf("web: parse content for %s: %w", name, err)
		}
		pages[name] = tpl
	}

	partials := template.New("partials").Funcs(funcs)
	for _, path := range partialFiles {
		body, err := templatesFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("web: read partial %s: %w", path, err)
		}
		if _, err := partials.Parse(string(body)); err != nil {
			return nil, fmt.Errorf("web: parse partial %s: %w", path, err)
		}
	}

	return &Server{
		logger:        logger,
		keys:          deps.Keys,
		tasks:         deps.Tasks,
		sessions:      deps.Sessions,
		handoffs:      deps.Handoffs,
		artifacts:     deps.Artifacts,
		manager:       deps.Manager,
		pages:         pages,
		partials:      partials,
		masterKeyPath: masterKeyPath,
	}, nil
}

// Handler returns the configured chi router. Mount under "/" — there is no
// path prefix.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tasks", http.StatusFound)
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	})

	r.Route("/keys", func(r chi.Router) {
		r.Get("/", s.handleKeysIndex)
		r.Post("/", s.handleKeysCreate)
		r.Get("/new", s.handleKeysNewDialog)
		r.Get("/dialog/close", s.handleKeysDialogClose)
		r.Post("/check-all", s.handleKeysCheckAll)
		r.Get("/{id}/edit", s.handleKeysEditDialog)
		r.Put("/{id}", s.handleKeysUpdate)
		r.Delete("/{id}", s.handleKeysDelete)
		r.Post("/{id}/check", s.handleKeysCheck)
	})

	r.Route("/tasks", func(r chi.Router) {
		r.Get("/", s.handleTasksIndex)
		r.Post("/", s.handleTasksCreate)
		r.Get("/new", s.handleTasksNewDialog)
		r.Get("/dialog/close", s.handleTasksDialogClose)
		r.Get("/{id}", s.handleTaskDetail)
	})

	r.Route("/sessions", func(r chi.Router) {
		r.Get("/{id}", s.handleSessionChat)
		r.Get("/{id}/messages", s.handleSessionMessages)
		r.Post("/{id}/messages", s.handleSessionSend)
		r.Post("/{id}/sync", s.handleSessionSync)
		r.Post("/{id}/rotate", s.handleSessionRotate)
		r.Post("/{id}/snap", s.handleSessionSnap)
		r.Get("/{id}/files", s.handleSessionFiles)
	})

	r.Route("/artifacts", func(r chi.Router) {
		r.Get("/{id}/raw", s.handleArtifactRaw)
		r.Get("/{id}/download", s.handleArtifactDownload)
	})

	r.Route("/handoffs", func(r chi.Router) {
		r.Get("/{id}", s.handleHandoffDetail)
	})

	return r
}

// --- /keys handlers (unchanged from PR-1) ---

func (s *Server) handleKeysIndex(w http.ResponseWriter, r *http.Request) {
	all, err := s.keys.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPage(w, r, "keys_index", pageData{
		Title:         "Keys",
		Active:        "keys",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Keys:          all,
		Flash:         r.URL.Query().Get("flash"),
	})
}

func (s *Server) handleKeysNewDialog(w http.ResponseWriter, _ *http.Request) {
	s.renderPartial(w, "keys_form", dialogData{Editing: false})
}

func (s *Server) handleKeysDialogClose(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "")
}

func (s *Server) handleKeysEditDialog(w http.ResponseWriter, r *http.Request) {
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
	s.renderPartial(w, "keys_form", dialogData{Editing: true, Key: k})
}

func (s *Server) handleKeysCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	in := keys.CreateInput{
		Label:  r.PostFormValue("label"),
		Plan:   keys.Plan(strings.TrimSpace(r.PostFormValue("plan"))),
		APIKey: r.PostFormValue("api_key"),
		Notes:  r.PostFormValue("notes"),
	}
	if _, err := s.keys.Create(r.Context(), in); err != nil {
		if errors.Is(err, keys.ErrDuplicateKey) {
			s.renderPartial(w, "keys_form", dialogData{Editing: false, Error: "This API key is already in the pool."})
			return
		}
		s.logger.Warn("create key failed", "err", err)
		s.renderPartial(w, "keys_form", dialogData{Editing: false, Error: err.Error()})
		return
	}
	s.redirect(w, r, "/keys?flash=Key+added.")
}

func (s *Server) handleKeysUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	in := keys.UpdateInput{
		Label: r.PostFormValue("label"),
		Plan:  keys.Plan(strings.TrimSpace(r.PostFormValue("plan"))),
		Notes: r.PostFormValue("notes"),
	}
	if err := s.keys.Update(r.Context(), id, in); err != nil {
		if errors.Is(err, keys.ErrNotFound) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		k, _ := s.keys.Get(r.Context(), id)
		s.logger.Warn("update key failed", "id", id, "err", err)
		s.renderPartial(w, "keys_form", dialogData{Editing: true, Key: k, Error: err.Error()})
		return
	}
	s.redirect(w, r, "/keys?flash=Key+updated.")
}

func (s *Server) handleKeysDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.keys.Delete(r.Context(), id); err != nil && !errors.Is(err, keys.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	all, err := s.keys.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPartial(w, "keys_table", pageData{Keys: all})
}

// handleKeysCheck validates a single key on demand and re-renders the keys
// page so the user immediately sees the updated status pill.
func (s *Server) handleKeysCheck(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := s.manager.CheckKey(r.Context(), id)
	if errors.Is(err, keys.ErrNotFound) {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flash := fmt.Sprintf("%s: %s", res.Label, res.Status)
	if res.Error != "" {
		flash = fmt.Sprintf("%s: %s — %s", res.Label, res.Status, res.Error)
	}
	s.redirect(w, r, "/keys?flash="+urlEncode(flash))
}

// handleKeysCheckAll runs the validator against every key in the pool and
// renders a flash summarising the results.
func (s *Server) handleKeysCheckAll(w http.ResponseWriter, r *http.Request) {
	results, err := s.manager.CheckAllKeys(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flash := summariseCheckResults(results)
	s.redirect(w, r, "/keys?flash="+urlEncode(flash))
}

func summariseCheckResults(results []manager.CheckResult) string {
	if len(results) == 0 {
		return "No keys to check."
	}
	counts := map[devin.ValidateStatus]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	parts := []string{fmt.Sprintf("Checked %d keys", len(results))}
	for _, status := range []devin.ValidateStatus{
		devin.ValidateValid, devin.ValidateQuotaExhausted, devin.ValidateRateLimited,
		devin.ValidateUnauthorized, devin.ValidateNetworkError, devin.ValidateAPIError,
	} {
		if n := counts[status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	return strings.Join(parts, " · ")
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}

// --- /tasks handlers ---

func (s *Server) handleTasksIndex(w http.ResponseWriter, r *http.Request) {
	all, err := s.tasks.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPage(w, r, "tasks_index", pageData{
		Title:         "Tasks",
		Active:        "tasks",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Tasks:         all,
		Flash:         r.URL.Query().Get("flash"),
	})
}

func (s *Server) handleTasksNewDialog(w http.ResponseWriter, _ *http.Request) {
	s.renderPartial(w, "task_form", dialogData{})
}

func (s *Server) handleTasksDialogClose(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "")
}

func (s *Server) handleTasksCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	in := manager.StartTaskInput{
		Title:  r.PostFormValue("title"),
		Prompt: r.PostFormValue("prompt"),
	}
	result, err := s.manager.StartTask(r.Context(), in)
	if err != nil {
		msg := err.Error()
		switch {
		case errors.Is(err, manager.ErrNoActiveKey):
			msg = "No active keys available. Add one on the Keys page first."
		case errors.Is(err, devin.ErrQuotaExhausted):
			msg = "Selected key has no quota left. Try again after another key becomes active."
		case errors.Is(err, devin.ErrUnauthorized):
			msg = "Devin rejected the API key (unauthorized). Check the key value on the Keys page."
		}
		s.logger.Warn("start task failed", "err", err)
		s.renderPartial(w, "task_form", dialogData{Error: msg})
		return
	}
	s.redirect(w, r, "/sessions/"+result.Session.ID)
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.tasks.Get(r.Context(), id)
	if errors.Is(err, tasks.ErrNotFound) {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sess, err := s.sessions.ListByTask(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	handoffList, err := s.handoffs.ListByTask(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]sessionRow, 0, len(sess))
	for _, sx := range sess {
		k, err := s.keys.Get(r.Context(), sx.KeyID)
		label := sx.KeyID
		if err == nil {
			label = k.Label
		}
		rows = append(rows, sessionRow{
			ID:             sx.ID,
			KeyLabel:       label,
			DevinSessionID: sx.DevinSessionID,
			Status:         string(sx.Status),
			StartedAt:      sx.StartedAt,
		})
	}
	s.renderPage(w, r, "task_detail", pageData{
		Title:         task.Title,
		Active:        "tasks",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Task:          task,
		Sessions:      sess,
		SessionRows:   rows,
		Handoffs:      handoffList,
	})
}

// --- /sessions handlers ---

func (s *Server) handleSessionChat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	view, err := s.loadSessionView(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Flash = r.URL.Query().Get("flash")
	s.renderPage(w, r, "session_chat", view)
}

// handleSessionMessages returns the chat_messages partial for HTMX polling.
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	view, err := s.loadSessionView(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPartial(w, "chat_messages", view)
}

func (s *Server) handleSessionSend(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	text := r.PostFormValue("text")
	if err := s.manager.SendFollowUp(r.Context(), id, text); err != nil {
		s.logger.Warn("send follow up failed", "session_id", id, "err", err)
		// Render the chat stream regardless so the user sees current state;
		// the error is surfaced in the log. A future PR will add a flash banner.
	}
	view, err := s.loadSessionView(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPartial(w, "chat_messages", view)
}

func (s *Server) handleSessionSync(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.manager.SyncSession(r.Context(), id); err != nil {
		s.logger.Warn("manual sync failed", "session_id", id, "err", err)
	}
	view, err := s.loadSessionView(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderPartial(w, "chat_messages", view)
}

// handleSessionRotate is the user-driven "Rotate now" button. It mints a
// fresh session on the next active key, seeds it with a handoff prompt, and
// redirects the browser to the new chat.
func (s *Server) handleSessionRotate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := s.manager.ForceRotate(r.Context(), id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, keys.ErrNoActiveKey) {
			s.redirect(w, r, "/sessions/"+id+"?flash="+urlEncode("No replacement key available. Add one or wait for a cooldown to expire."))
			return
		}
		s.logger.Warn("rotate failed", "session_id", id, "err", err)
		s.redirect(w, r, "/sessions/"+id+"?flash="+urlEncode("Rotation failed: "+err.Error()))
		return
	}
	s.redirect(w, r, "/sessions/"+res.ToSession.ID+"?flash="+urlEncode("Rotated to "+res.NewKey.Label+"."))
}

// handleSessionSnap sends the predefined "take a screenshot" prompt to the
// session's Devin instance. The reply is picked up on the next sync and the
// downloader saves any image attachment so the chat view can render it inline.
func (s *Server) handleSessionSnap(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.manager.SnapDesktop(r.Context(), id); err != nil {
		s.logger.Warn("snap desktop failed", "session_id", id, "err", err)
		s.redirect(w, r, "/sessions/"+id+"?flash="+urlEncode("Snap failed: "+err.Error()))
		return
	}
	s.redirect(w, r, "/sessions/"+id+"?flash="+urlEncode("Asked Devin for a screenshot. It will appear in chat shortly."))
}

// handleSessionFiles renders the per-session artifact gallery.
func (s *Server) handleSessionFiles(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessions.Get(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	task, err := s.tasks.Get(r.Context(), sess.TaskID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var list []artifacts.Artifact
	if s.artifacts != nil {
		list, err = s.artifacts.ListBySession(r.Context(), sess.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.renderPage(w, r, "session_files", pageData{
		Title:         "Files \u00b7 " + task.Title,
		Active:        "tasks",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Task:          task,
		Session:       sess,
		Artifacts:     list,
		Flash:         r.URL.Query().Get("flash"),
	})
}

// handleArtifactRaw streams the on-disk artifact body with the recorded
// Content-Type. Used for inline image rendering in the chat bubble.
func (s *Server) handleArtifactRaw(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, false)
}

// handleArtifactDownload behaves like handleArtifactRaw but adds a
// Content-Disposition header so the browser saves the file rather than
// rendering it inline.
func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, true)
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, asAttachment bool) {
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
		http.Error(w, "artifact not yet ready ("+string(a.Status)+")", http.StatusAccepted)
		return
	}
	if a.ContentType != "" {
		w.Header().Set("Content-Type", a.ContentType)
	}
	if asAttachment {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, a.Filename))
	}
	http.ServeFile(w, r, a.LocalPath)
}

// handleHandoffDetail renders the full markdown body of a handoff so the user
// can inspect what context was carried over.
func (s *Server) handleHandoffDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	hoff, err := s.findHandoffByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, handoffs.ErrNotFound) {
			http.Error(w, "handoff not found", http.StatusNotFound)
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = io.WriteString(w, hoff.Markdown)
}

// findHandoffByID is a thin lookup helper for the detail handler. We don't
// expose Get directly on handoffs.Repo yet — the only callers today need
// either the chain for a task or the handoff that begat a specific session.
func (s *Server) findHandoffByID(ctx context.Context, handoffID string) (handoffs.Handoff, error) {
	// Walk every task's handoff chain until we find the id. Cheap because
	// this is only called when the user clicks an explicit "view" link.
	tasksAll, err := s.tasks.List(ctx)
	if err != nil {
		return handoffs.Handoff{}, err
	}
	for _, t := range tasksAll {
		list, err := s.handoffs.ListByTask(ctx, t.ID)
		if err != nil {
			return handoffs.Handoff{}, err
		}
		for _, h := range list {
			if h.ID == handoffID {
				return h, nil
			}
		}
	}
	return handoffs.Handoff{}, handoffs.ErrNotFound
}

func (s *Server) loadSessionView(ctx context.Context, id string) (pageData, error) {
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return pageData{}, err
	}
	task, err := s.tasks.Get(ctx, sess.TaskID)
	if err != nil {
		return pageData{}, err
	}
	key, err := s.keys.Get(ctx, sess.KeyID)
	if err != nil {
		return pageData{}, err
	}
	msgs, err := s.sessions.ListMessages(ctx, sess.ID)
	if err != nil {
		return pageData{}, err
	}

	// Composer disabled when the session is in a terminal state.
	composer := composerData{}
	switch sess.Status {
	case sessions.StatusCompleted, sessions.StatusFailed,
		sessions.StatusQuotaExhausted, sessions.StatusHandoffDone:
		composer.Disabled = true
		composer.Hint = "This session is closed. Open a new task to continue."
	}

	// Surface the inbound handoff (if any) so the chat view shows where
	// this session was resumed from.
	var inbound handoffs.Handoff
	if hoff, herr := s.handoffs.GetForSession(ctx, sess.ID); herr == nil {
		inbound = hoff
	} else if !errors.Is(herr, handoffs.ErrNotFound) {
		s.logger.Warn("load inbound handoff", "session_id", sess.ID, "err", herr)
	}

	// And the outbound handoff (where this dying session rotated to).
	var outbound handoffs.Handoff
	outboundList, err := s.handoffs.ListByTask(ctx, sess.TaskID)
	if err == nil {
		for _, h := range outboundList {
			if h.FromSessionID == sess.ID && h.ToSessionID != "" {
				outbound = h
				break
			}
		}
	}

	// Group artifacts by remote URL so chat bubbles can render inline
	// images for any URL they reference.
	artByURL := map[string]artifacts.Artifact{}
	var art []artifacts.Artifact
	if s.artifacts != nil {
		art, err = s.artifacts.ListBySession(ctx, sess.ID)
		if err != nil {
			return pageData{}, err
		}
		for _, a := range art {
			artByURL[a.RemoteURL] = a
		}
	}

	// Decorate each message with the artifact rows referenced in its body.
	decorated := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		mv := messageView{Message: m}
		for _, eu := range artifacts.ExtractURLs(m.Content) {
			if a, ok := artByURL[eu.URL]; ok {
				mv.Artifacts = append(mv.Artifacts, a)
			}
		}
		decorated = append(decorated, mv)
	}

	return pageData{
		Title:           "Chat · " + task.Title,
		Active:          "tasks",
		Version:         version.Version,
		MasterKeyPath:   s.masterKeyPath,
		Task:            task,
		Session:         sess,
		Key:             key,
		Messages:        msgs,
		MessageViews:    decorated,
		Artifacts:       art,
		StatusLabel:     string(sess.Status),
		Composer:        composer,
		InboundHandoff:  inbound,
		OutboundHandoff: outbound,
	}, nil
}

// --- helpers ---

// redirect handles both the htmx and the plain-form fallback case.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) renderPage(w http.ResponseWriter, _ *http.Request, pageName string, data pageData) {
	tpl, ok := s.pages[pageName]
	if !ok {
		http.Error(w, "unknown page "+pageName, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("render", "page", pageName, "err", err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, partialName string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partials.ExecuteTemplate(w, partialName, data); err != nil {
		s.logger.Error("partial render", "partial", partialName, "err", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, _ *http.Request, err error) {
	s.logger.Error("server error", "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.logger.Debug("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "took", time.Since(start))
	})
}

// pageData is the common shape passed to renders. Fields are populated only
// where relevant for the current view; templates check zero values where
// needed.
type pageData struct {
	Title         string
	Active        string
	Version       string
	MasterKeyPath string
	Flash         string

	// Keys index.
	Keys []keys.Key

	// Tasks index.
	Tasks []tasks.Task

	// Task detail.
	Task        tasks.Task
	Sessions    []sessions.Session
	SessionRows []sessionRow
	Handoffs    []handoffs.Handoff

	// Session chat / files.
	Session         sessions.Session
	Key             keys.Key
	Messages        []sessions.Message
	MessageViews    []messageView
	Artifacts       []artifacts.Artifact
	StatusLabel     string
	Composer        composerData
	InboundHandoff  handoffs.Handoff
	OutboundHandoff handoffs.Handoff
}

// messageView pairs a session message with any artifacts referenced from its
// body. Templates iterate MessageViews instead of raw Messages so chat
// bubbles can render inline images and download links right under the prose.
type messageView struct {
	Message   sessions.Message
	Artifacts []artifacts.Artifact
}

// dialogData is passed to dialog-style partial templates (keys_form, task_form).
type dialogData struct {
	Editing bool
	Key     keys.Key
	Error   string
}

// sessionRow flattens the join of sessions + keys for the task detail table.
type sessionRow struct {
	ID             string
	KeyLabel       string
	DevinSessionID string
	Status         string
	StartedAt      time.Time
}

// composerData controls the chat input form on /sessions/{id}.
type composerData struct {
	Disabled bool
	Hint     string
}

// Run is a convenience wrapper that starts the HTTP server and blocks until
// the supplied context is cancelled.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.logger.Info("listening", "addr", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
