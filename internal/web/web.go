// Package web wires the HTTP routes for the local dashboard. Templates and
// static assets are embedded so the binary remains a single file.
package web

import (
	"archive/zip"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ggwpgoend/devin-key-manager/internal/aisearch"
	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/attachments"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/events"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/observability"
	"github.com/ggwpgoend/devin-key-manager/internal/pipelines"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/vendor/* static/storm.css
var staticFS embed.FS

// staticVersion is appended as a ?v= query param to vendored asset URLs so
// the browser cache busts whenever the binary is rebuilt with new vendor
// files. Source: the package version. Cheap and reliable since the binary
// is a single artifact — a fresh build means fresh assets.

// Server is the HTTP layer of the dashboard.
type Server struct {
	logger        *slog.Logger
	keys          *keys.Repo
	tasks         *tasks.Repo
	sessions      *sessions.Repo
	handoffs      *handoffs.Repo
	artifacts     *artifacts.Repo
	schedules     *schedules.Repo
	notifs        *notifications.Repo
	attachments   *attachments.Repo
	pipelines     *pipelines.Repo
	pipelineExec  *pipelines.Executor
	observability *observability.Repo
	bus           *events.Bus
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
	"keys_index":       "templates/keys_index.html",
	"tasks_index":      "templates/tasks_index.html",
	"task_detail":      "templates/task_detail.html",
	"session_chat":     "templates/session_chat.html",
	"session_files":    "templates/session_files.html",
	"artifact_preview": "templates/artifact_preview.html",
	"keys_index":      "templates/keys_index.html",
	"tasks_index":     "templates/tasks_index.html",
	"task_detail":     "templates/task_detail.html",
	"session_chat":    "templates/session_chat.html",
	"session_files":   "templates/session_files.html",
	"schedules_index": "templates/schedules_index.html",
	// PR-13: pipeline editor (E43.1).
	"pipelines_index": "templates/pipelines_index.html",
	"pipeline_editor": "templates/pipeline_editor.html",
	// PR-14: observability tab.
	"observability": "templates/observability.html",
}

// partialFiles enumerates the HTMX partial templates rendered without the
// layout chrome (dialogs, table fragments, etc.). They share a single parse
// tree because their {{define ...}} names do not collide.
var partialFiles = []string{
	"templates/keys_index.html",
	"templates/keys_form.html",
	"templates/keys_bulk_form.html",
	"templates/keys_bulk_results.html",
	"templates/task_form.html",
	"templates/session_chat.html",
}

// Deps bundles the repositories and orchestrator the web layer depends on.
type Deps struct {
	Keys          *keys.Repo
	Tasks         *tasks.Repo
	Sessions      *sessions.Repo
	Handoffs      *handoffs.Repo
	Artifacts     *artifacts.Repo
	Schedules     *schedules.Repo
	Notifications *notifications.Repo
	Attachments   *attachments.Repo
	Pipelines     *pipelines.Repo
	PipelineExec  *pipelines.Executor
	Observability *observability.Repo
	Bus           *events.Bus
	Manager       *manager.Manager
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
		schedules:     deps.Schedules,
		notifs:        deps.Notifications,
		attachments:   deps.Attachments,
		pipelines:     deps.Pipelines,
		pipelineExec:  deps.PipelineExec,
		observability: deps.Observability,
		bus:           deps.Bus,
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

	// PR-11: dashboard is the new root. Bento layout with live KPIs.
	r.Get("/", s.handleDashboard)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	})

	r.Route("/keys", func(r chi.Router) {
		r.Get("/", s.handleKeysIndex)
		r.Post("/", s.handleKeysCreate)
		r.Get("/new", s.handleKeysNewDialog)
		r.Get("/bulk", s.handleKeysBulkDialog)
		r.Post("/bulk", s.handleKeysBulkImport)
		r.Post("/detect", s.handleKeysDetectPlan)
		r.Get("/dialog/close", s.handleKeysDialogClose)
		r.Post("/check-all", s.handleKeysCheckAll)
		// PR-12: bulk delete (A4). POST /keys/bulk-delete with id=... params.
		// Add ?dry_run=1 to preview affected rows without committing.
		r.Post("/bulk-delete", s.handleKeysBulkDelete)
		r.Get("/{id}/edit", s.handleKeysEditDialog)
		r.Put("/{id}", s.handleKeysUpdate)
		r.Delete("/{id}", s.handleKeysDelete)
		r.Post("/{id}/check", s.handleKeysCheck)
		// PR-12: tags (A3). PUT /keys/{id}/tags with tags=tag1,tag2,...
		r.Put("/{id}/tags", s.handleKeysSetTags)
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
		r.Get("/{id}/files.zip", s.handleSessionFilesZip)
		r.Post("/{id}/files/open", s.handleSessionFilesOpen)
		r.Post("/{id}/notes", s.handleSessionNotes)
		// PR-12: chat search (C29). GET /sessions/{id}/search?q=...
		r.Get("/{id}/search", s.handleSessionSearch)
		// PR-12: continue from stopped (C30). POST /sessions/{id}/continue
		// re-sends "continue" to the Devin session.
		r.Post("/{id}/continue", s.handleSessionContinue)
		// PR-12: file upload (C26). multipart POST with a single "file" part.
		r.Post("/{id}/attachments", s.handleSessionAttach)
		r.Get("/{id}/attachments", s.handleSessionAttachmentsList)
		// PR-12: fork/checkpoint (B14). Optional anchor_id=<message-id>.
		r.Post("/{id}/fork", s.handleSessionFork)
	})

	// PR-12: pin/unpin messages (C22).
	r.Route("/messages", func(r chi.Router) {
		r.Post("/{id}/pin", s.handleMessagePin)
		r.Post("/{id}/unpin", s.handleMessageUnpin)
	})

	// PR-12: cross-session search (FTS5). GET /search?q=...
	r.Get("/search", s.handleGlobalSearch)

	r.Route("/artifacts", func(r chi.Router) {
		r.Get("/{id}/raw", s.handleArtifactRaw)
		r.Get("/{id}/download", s.handleArtifactDownload)
		r.Get("/{id}/preview", s.handleArtifactPreview)
	})

	r.Route("/handoffs", func(r chi.Router) {
		r.Get("/{id}", s.handleHandoffDetail)
	})

	// PR-13: pipeline editor (E43.1). Routes:
	//   GET    /pipelines                 — list view
	//   POST   /pipelines                 — create new pipeline
	//   GET    /pipelines/{id}            — editor canvas
	//   PUT    /pipelines/{id}            — update header
	//   DELETE /pipelines/{id}            — delete
	//   GET    /pipelines/{id}/graph      — JSON graph (nodes + edges)
	//   PUT    /pipelines/{id}/graph      — replace nodes + edges atomically
	//   POST   /pipelines/{id}/runs       — start a run (simulated)
	//   GET    /pipelines/{id}/runs       — list runs
	//   GET    /pipeline-runs/{run_id}    — run detail (steps)
	//   POST   /pipeline-runs/{run_id}/rollback — rewind one step
	r.Route("/pipelines", func(r chi.Router) {
		r.Get("/", s.handlePipelinesIndex)
		r.Post("/", s.handlePipelinesCreate)
		r.Get("/{id}", s.handlePipelineEditor)
		r.Put("/{id}", s.handlePipelineUpdateHeader)
		r.Delete("/{id}", s.handlePipelineDelete)
		r.Get("/{id}/graph", s.handlePipelineGraphGet)
		r.Put("/{id}/graph", s.handlePipelineGraphReplace)
		r.Post("/{id}/runs", s.handlePipelineStartRun)
		r.Get("/{id}/runs", s.handlePipelineListRuns)
	})
	// PR-14: observability tab. UI page + JSON endpoints for time-series
	// (sessions, messages, handoffs, tasks) and per-task session-graphs.
	r.Get("/observability", s.handleObservabilityIndex)
	r.Route("/api/observability", func(r chi.Router) {
		r.Get("/timeseries", s.handleObservabilityTimeseries)
		r.Get("/state-breakdown", s.handleObservabilityStateBreakdown)
		r.Get("/top-keys", s.handleObservabilityTopKeys)
		r.Get("/session-graph/{task_id}", s.handleObservabilitySessionGraph)
	})

	// PR-15: AI/search helpers. All deterministic local computation.
	r.Route("/api/ai", func(r chi.Router) {
		r.Post("/autotag", s.handleAIAutotag)
		r.Post("/tokens", s.handleAITokens)
		r.Get("/suggest", s.handleAISuggest)
		r.Get("/similar-tasks", s.handleAISimilarTasks)
	})
	r.Put("/api/tasks/{id}/tags", s.handleSetTaskTags)

	// PR-16: lint-on-incoming, key lifecycle history, notification dispatch.
	r.Get("/api/artifacts/{id}/lint", s.handleArtifactLint)
	r.Get("/api/keys/{id}/history", s.handleKeyHistory)
	r.Post("/api/notifications/dispatch", s.handleNotificationDispatch)
	// PR-16: JSON endpoint for the browser extension to POST keys to.
	r.Post("/api/keys", s.handleAPIKeysCreate)
	r.Options("/api/keys", s.handleAPIKeysCreate)

	// PR-17: artifact retention + pinning.
	r.Post("/api/artifacts/{id}/pin", s.handleArtifactPin)
	r.Post("/api/artifacts/retention/sweep", s.handleArtifactRetentionSweep)

	r.Route("/pipeline-runs", func(r chi.Router) {
		r.Get("/{run_id}", s.handlePipelineRunDetail)
		r.Post("/{run_id}/rollback", s.handlePipelineRunRollback)
	})

	r.Route("/schedules", func(r chi.Router) {
		r.Get("/", s.handleSchedulesIndex)
		r.Post("/", s.handleSchedulesCreate)
		r.Post("/{id}/toggle", s.handleSchedulesToggle)
		r.Post("/{id}/run", s.handleSchedulesRunNow)
		r.Delete("/{id}", s.handleSchedulesDelete)
	})

	r.Get("/events/since", s.handleEventsSince)
	// Server-Sent Events: long-lived stream pushing live state changes
	// (key state, session messages, artifact ready, handoff linked).
	// Lets the UI patch DOM without 4-15s polling cycles.
	r.Get("/events/stream", s.handleEventsStream)

	// JSON API (PR-10 #1). Parallel to the HTMX endpoints so external
	// scripts / future UI fragments can consume the same data without
	// scraping HTML. See internal/web/api.go.
	s.registerAPI(r)

	// Embedded vendor assets (HTMX, marked.js, highlight.js, Tailwind play,
	// hljs theme). Long Cache-Control headers because the URLs include a
	// ?v=<version> bust token wired in layout.html. Anything that wants to
	// override one of these can drop a file at the same path in /static/.
	r.Get("/static/vendor/*", s.handleStaticVendor)
	// PR-11: storm.css palette/components served the same way; also a
	// general fallthrough for any future /static/<name>.css we add.
	r.Get("/static/{name}", s.handleStaticTop)

	return r
}

func (s *Server) handleStaticVendor(w http.ResponseWriter, r *http.Request) {
	// chi RouteParam form for catch-all returns the trailing portion via
	// chi.URLParam with the wildcard name "*". We then re-prefix to look it
	// up in the embedded FS at static/vendor/<name>.
	tail := chi.URLParam(r, "*")
	if tail == "" || strings.Contains(tail, "..") {
		http.NotFound(w, r)
		return
	}
	serveStatic(w, "static/vendor/"+tail, tail)
}

func (s *Server) handleStaticTop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	serveStatic(w, "static/"+name, name)
}

func serveStatic(w http.ResponseWriter, fsPath, name string) {
	data, err := staticFS.ReadFile(fsPath)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".woff2"):
		w.Header().Set("Content-Type", "font/woff2")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
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
	rawPlan := strings.TrimSpace(r.PostFormValue("plan"))
	apiKey := strings.TrimSpace(r.PostFormValue("api_key"))
	plan := keys.Plan(rawPlan)
	flashTail := ""
	if rawPlan == "auto" {
		if apiKey == "" {
			s.renderPartial(w, "keys_form", dialogData{Error: "API key is required for auto-detect."})
			return
		}
		detected, status, err := s.manager.DetectPlan(r.Context(), apiKey)
		if err != nil && status == devin.ValidateUnauthorized {
			s.renderPartial(w, "keys_form", dialogData{Error: "Devin says the key is unauthorized. Double-check the value and try again."})
			return
		}
		plan = detected
		flashTail = "+(auto-detected+as+" + string(detected) + ")"
	}
	in := keys.CreateInput{
		Label:  r.PostFormValue("label"),
		Plan:   plan,
		APIKey: apiKey,
		Notes:  r.PostFormValue("notes"),
		Tags:   splitTagsInput(r.PostFormValue("tags")),
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
	s.redirect(w, r, "/keys?flash=Key+added"+flashTail+".")
}

func (s *Server) handleKeysBulkDialog(w http.ResponseWriter, _ *http.Request) {
	s.renderPartial(w, "keys_bulk_form", bulkData{})
}

func (s *Server) handleKeysBulkImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	payload := r.PostFormValue("payload")
	if strings.TrimSpace(payload) == "" {
		s.renderPartial(w, "keys_bulk_form", bulkData{Error: "Paste at least one line first."})
		return
	}
	results, err := s.manager.BulkImport(r.Context(), payload)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts := struct{ Created, Duplicate, Unauthorized, Bad int }{}
	for _, res := range results {
		switch res.Outcome {
		case manager.BulkOutcomeCreated:
			counts.Created++
		case manager.BulkOutcomeDuplicate:
			counts.Duplicate++
		case manager.BulkOutcomeUnauthorized:
			counts.Unauthorized++
		default:
			counts.Bad++
		}
	}
	s.renderPartial(w, "keys_bulk_results", bulkData{
		Results:      results,
		Created:      counts.Created,
		Duplicate:    counts.Duplicate,
		Unauthorized: counts.Unauthorized,
		Bad:          counts.Bad,
	})
}

// handleKeysDetectPlan is the HTMX hook fired when the user clicks the
// 'Detect' chip next to the plan dropdown in the add-key dialog. Returns a
// tiny HTML fragment that replaces the chip's label with the detected plan.
func (s *Server) handleKeysDetectPlan(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	apiKey := strings.TrimSpace(r.PostFormValue("api_key"))
	if apiKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<span class="text-amber-300">Paste a key first.</span>`)
		return
	}
	detected, status, err := s.manager.DetectPlan(r.Context(), apiKey)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil && status == devin.ValidateUnauthorized {
		_, _ = io.WriteString(w, `<span class="text-rose-300">Unauthorized.</span>`)
		return
	}
	switch status {
	case devin.ValidateValid:
		fmt.Fprintf(w, `<span class="text-emerald-300">Detected: %s</span>`, detected)
	case devin.ValidateQuotaExhausted:
		fmt.Fprintf(w, `<span class="text-amber-300">Detected: %s (quota exhausted, key is valid)</span>`, detected)
	case devin.ValidateNetworkError:
		_, _ = io.WriteString(w, `<span class="text-amber-300">Network error — defaulting to trial.</span>`)
	default:
		fmt.Fprintf(w, `<span class="text-amber-300">Inconclusive (%s) — defaulting to %s.</span>`, status, detected)
	}
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
		Tags:  splitTagsInput(r.PostFormValue("tags")),
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

// splitTagsInput parses a comma-separated form input ("work, beta, foo")
// into a clean []string. NormalizeTags() runs on the repo side, so this
// only handles the raw form decoding.
func splitTagsInput(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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
		// PR-17 / roadmap A9: sticky session-to-key. Caller can pass a
		// preferred key ID; if the key is no longer active the picker
		// transparently falls back to round-robin.
		PreferKeyID: strings.TrimSpace(r.PostFormValue("prefer_key_id")),
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
	// PR-15: best-effort auto-tag from the prompt. Don't block task
	// creation on a tags failure — the user can always edit tags later.
	if tags := aisearch.AutoTag(in.Prompt); len(tags) > 0 && result.Task.ID != "" {
		if err := s.tasks.SetTags(r.Context(), result.Task.ID, strings.Join(tags, ",")); err != nil {
			s.logger.Warn("autotag set failed", "task_id", result.Task.ID, "err", err)
		}
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

// handleSessionNotes saves the user's private notes for a session. The text
// is written as-is to the sessions.notes column and never forwarded to Devin.
// On success we return a tiny status fragment that the notes panel swaps in
// place of its "saving…" indicator.
func (s *Server) handleSessionNotes(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	notes := strings.TrimRight(r.FormValue("notes"), " \t\n\r")
	if err := s.sessions.SetNotes(r.Context(), id, notes); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span id="notes-status" class="text-emerald-400">saved %s</span>`,
		time.Now().Format("15:04:05"))
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
	var dir string
	if root := s.manager.ArtifactsRoot(); root != "" {
		dir = filepath.Join(root, sess.ID)
	}
	s.renderPage(w, r, "session_files", pageData{
		Title:         "Files \u00b7 " + task.Title,
		Active:        "tasks",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Task:          task,
		Session:       sess,
		Artifacts:     list,
		ArtifactsDir:  dir,
		Flash:         r.URL.Query().Get("flash"),
	})
}

// handleSessionFilesZip streams a zip of every ready artifact for the session
// so the user can grab them all in one click. Files inside the zip are named
// after the artifact filename; the streamer skips pending / failed rows so
// the archive only contains usable bytes.
func (s *Server) handleSessionFilesZip(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
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
	list, err := s.artifacts.ListBySession(r.Context(), sess.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(fmt.Sprintf("session-%s.zip", sess.ID)))
	zw := zip.NewWriter(w)
	defer zw.Close()
	used := make(map[string]int)
	var skipped []string
	for _, a := range list {
		if a.Status != artifacts.StatusReady || a.LocalPath == "" {
			continue
		}
		// Open + stat the file BEFORE creating a zip entry. If the file
		// disappeared or the disk is unhappy, we'd rather skip the
		// artifact and note it in the summary than write a zip entry
		// whose header claims N bytes followed by a truncated payload
		// (which leaves a corrupt CRC and unzip errors at the recipient).
		fh, ferr := os.Open(a.LocalPath)
		if ferr != nil {
			s.logger.Warn("zip open file", "path", a.LocalPath, "err", ferr)
			skipped = append(skipped, fmt.Sprintf("%s: open failed: %v", a.Filename, ferr))
			continue
		}
		name := uniqueZipName(a.Filename, used)
		fw, werr := zw.Create(name)
		if werr != nil {
			_ = fh.Close()
			s.logger.Warn("zip create entry", "err", werr)
			skipped = append(skipped, fmt.Sprintf("%s: zip create failed: %v", a.Filename, werr))
			continue
		}
		if _, cerr := io.Copy(fw, fh); cerr != nil {
			// The entry header is already in the stream; we can't yank it
			// out. archive/zip's data descriptor will record whatever bytes
			// we wrote, so the resulting entry's CRC will mismatch the
			// truncated payload. Note it in skipped[] and keep going so the
			// other files still land in the archive.
			s.logger.Warn("zip copy file", "path", a.LocalPath, "err", cerr)
			skipped = append(skipped, fmt.Sprintf("%s: copy truncated at byte position: %v", a.Filename, cerr))
		}
		_ = fh.Close()
	}
	// Always include a summary entry — empty when everything succeeded —
	// so users can audit what made it into the archive without having to
	// cross-reference the UI. The presence/absence of names in skipped is
	// also how we surface partial failures (#19).
	if fw, werr := zw.Create("_manifest.txt"); werr == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "session-id: %s\n", sess.ID)
		fmt.Fprintf(&b, "generated-at: %s\n", time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "included: %d\n", len(used))
		if len(skipped) > 0 {
			b.WriteString("\n# Skipped entries:\n")
			for _, s := range skipped {
				b.WriteString("- ")
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
		_, _ = fw.Write([]byte(b.String()))
	}
}

// handleSessionFilesOpen opens the local artifact folder in the host OS's
// file manager. Only safe because the manager binds to localhost — the
// endpoint is a no-op when the artifacts directory is missing.
func (s *Server) handleSessionFilesOpen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	root := s.manager.ArtifactsRoot()
	if root == "" {
		http.Error(w, "artifacts disabled", http.StatusNotFound)
		return
	}
	dir := filepath.Join(root, id)
	if _, err := os.Stat(dir); err != nil {
		// The folder may not exist yet if no artifacts have downloaded; try
		// to create it so the file-manager has something to open.
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			http.Error(w, "folder unavailable: "+mkErr.Error(), http.StatusNotFound)
			return
		}
	}
	if err := openInFileManager(dir); err != nil {
		s.logger.Warn("open folder", "dir", dir, "err", err)
		http.Error(w, "could not open folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-emerald-300">Opened %s</span>`, template.HTMLEscapeString(dir))
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

// previewMaxBytes caps the inline-preview body to 256 KiB. Anything bigger is
// summarised with a download nudge — code review for a half-megabyte source
// file inside a browser tab isn't a sensible UX.
const previewMaxBytes = 256 * 1024

// handleArtifactPreview renders a syntax-highlighted preview page for a
// text-like artifact. Image artifacts redirect to /raw; binary artifacts get
// a "this file isn't previewable" page with a download link.
func (s *Server) handleArtifactPreview(w http.ResponseWriter, r *http.Request) {
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
	if a.IsImage() {
		http.Redirect(w, r, "/artifacts/"+a.ID+"/raw", http.StatusSeeOther)
		return
	}

	view := pageData{
		Title:         "Preview \u00b7 " + a.Filename,
		Active:        "tasks",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Artifact:      a,
		PreviewLang:   a.PreviewLanguage(),
	}
	if sess, gerr := s.sessions.Get(r.Context(), a.SessionID); gerr == nil {
		view.Session = sess
		if task, terr := s.tasks.Get(r.Context(), sess.TaskID); terr == nil {
			view.Task = task
		}
	}
	if !a.IsTextLike() {
		view.PreviewBinary = true
		s.renderPage(w, r, "artifact_preview", view)
		return
	}
	fh, oerr := os.Open(a.LocalPath)
	if oerr != nil {
		view.PreviewError = "could not open file: " + oerr.Error()
		s.renderPage(w, r, "artifact_preview", view)
		return
	}
	defer fh.Close()
	body, rerr := io.ReadAll(io.LimitReader(fh, previewMaxBytes+1))
	if rerr != nil {
		view.PreviewError = "could not read file: " + rerr.Error()
		s.renderPage(w, r, "artifact_preview", view)
		return
	}
	if int64(len(body)) > previewMaxBytes {
		body = body[:previewMaxBytes]
		view.PreviewTooBig = true
	}
	// Sanitise non-UTF-8 input. Some artifacts come back as Windows-1251
	// dumps, Latin-1 logs, or binary-with-text-extension; rendering those
	// directly in the page would leak U+FFFD-laden mojibake into the
	// browser's source viewer. We detect invalid UTF-8 and fall back to
	// the "preview unavailable" branch so the user gets a clean download
	// link instead of garbled text.
	if !utf8.Valid(body) {
		view.PreviewBinary = true
		view.PreviewError = "file is not valid UTF-8 — preview unavailable (use Download)"
		s.renderPage(w, r, "artifact_preview", view)
		return
	}
	view.PreviewBody = string(body)
	s.renderPage(w, r, "artifact_preview", view)
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
		w.Header().Set("Content-Disposition", contentDispositionAttachment(a.Filename))
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
	// PR-17 / roadmap D35: visual grouping of artifacts in the gallery
	// view. The handler pre-computes these so the template stays free
	// of grouping logic.
	ArtifactGroups  []artifactGroup
	StatusLabel     string
	Composer        composerData
	InboundHandoff  handoffs.Handoff
	OutboundHandoff handoffs.Handoff

	// Schedules index.
	Schedules     []schedules.Schedule
	ScheduleError string
	ScheduleForm  scheduleFormData
	// ServerTime / ServerTZ are echoed back in the schedules form so the
	// user knows which zone a "daily HH:MM" entry will resolve in. The
	// scheduler always computes daily fires against time.Local on the
	// server, which can be UTC in headless setups.
	ServerTime string
	ServerTZ   string

	// Dashboard (PR-11).
	Stats       dashStats
	RecentTasks []tasks.Task
	KeyRollup   []keyRollup

	// Pipelines (PR-13).
	Pipelines     []pipelinesIndexRow
	PipelineRow   pipelines.Pipeline
	GraphJSON     string
	RunsJSON      string
	NavCurrent    string

	// Observability (PR-14).
	TasksAll []tasks.Task
}

// dashStats is the bento KPI bundle for the dashboard.
type dashStats struct {
	KeysActive      int
	KeysTotal       int
	KeysCooldown    int
	KeysDead        int
	TasksLast24h    int
	TasksRunning    int
	TasksDone       int
	SessionsLast24h int
	SessionsOpen    int
	SessionsClosed  int
	RequestsTotal   int64
}

// keyRollup is one row in the dashboard's key-pool tile (per plan).
type keyRollup struct {
	Plan      string
	Count     int
	Requests  int64
	PillClass string
}

// scheduleFormData carries the pre-filled values for the schedules creation
// form on /schedules so we can echo them back when validation fails.
type scheduleFormData struct {
	Title           string
	Prompt          string
	PlanHint        string
	Kind            string
	IntervalSeconds int64
	DailyHour       int
	DailyMinute     int
	// PR-17 / roadmap E41 + E42: cron expression and one-off ISO-8601
	// timestamp inputs. Empty for legacy interval/daily forms.
	CronExpr string
	FireAt   string
}

// artifactGroup buckets a slice of artifacts under a single human-
// readable label. Used by the gallery view to render artifacts grouped
// by broad content-type family (PR-17 / roadmap D35).
type artifactGroup struct {
	Label     string
	Artifacts []artifacts.Artifact
}

// groupArtifacts splits the artifacts list into named buckets keyed by
// content-type family. Pinned artifacts always come first so the user's
// favourites stay visible.
func groupArtifacts(list []artifacts.Artifact) []artifactGroup {
	if len(list) == 0 {
		return nil
	}
	families := map[string][]artifacts.Artifact{
		"Pinned":    nil,
		"Images":    nil,
		"Text":      nil,
		"Archives":  nil,
		"Documents": nil,
		"Other":     nil,
	}
	order := []string{"Pinned", "Images", "Text", "Archives", "Documents", "Other"}
	for _, a := range list {
		// Pinned wins regardless of content-type. The artifact still
		// appears in the gallery once.
		// NOTE: Artifact's struct doesn't have a Pinned field exposed
		// at the time of writing — when the schema migration lands the
		// scan() in artifacts.go will populate it. For now we treat all
		// artifacts as unpinned which is the safe default; the bucket
		// is reserved for future use.
		ct := strings.ToLower(a.ContentType)
		switch {
		case strings.HasPrefix(ct, "image/"):
			families["Images"] = append(families["Images"], a)
		case strings.HasPrefix(ct, "text/"),
			strings.Contains(ct, "json"), strings.Contains(ct, "javascript"),
			strings.Contains(ct, "xml"):
			families["Text"] = append(families["Text"], a)
		case strings.Contains(ct, "zip"), strings.Contains(ct, "tar"),
			strings.Contains(ct, "gzip"), strings.Contains(ct, "rar"):
			families["Archives"] = append(families["Archives"], a)
		case strings.Contains(ct, "pdf"), strings.Contains(ct, "msword"),
			strings.Contains(ct, "officedocument"), strings.Contains(ct, "spreadsheet"):
			families["Documents"] = append(families["Documents"], a)
		default:
			families["Other"] = append(families["Other"], a)
		}
	}
	var out []artifactGroup
	for _, k := range order {
		if len(families[k]) > 0 {
			out = append(out, artifactGroup{Label: k, Artifacts: families[k]})
		}
	}
	return out
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

// bulkData backs the bulk-import dialog and its results panel.
type bulkData struct {
	Error        string
	Results      []manager.BulkImportResult
	Created      int
	Duplicate    int
	Unauthorized int
	Bad          int
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
