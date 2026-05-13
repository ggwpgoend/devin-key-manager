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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server is the HTTP layer of the dashboard.
type Server struct {
	logger        *slog.Logger
	keys          *keys.Repo
	pages         map[string]*template.Template
	partials      *template.Template
	masterKeyPath string
}

// pageContentFiles enumerates the content templates that render as full pages.
// Each entry is parsed alongside layout.html into its own *template.Template so
// that multiple pages can each define a {{define "content"}} block without
// clashing in a single shared parse tree.
var pageContentFiles = map[string]string{
	"keys_index": "templates/keys_index.html",
}

// partialFiles enumerates the HTMX partial templates rendered without the
// layout chrome (dialogs, table fragments, etc.). They share a single parse
// tree because their {{define ...}} names do not collide.
var partialFiles = []string{
	"templates/keys_index.html",
	"templates/keys_form.html",
}

// NewServer compiles templates and prepares the handler. masterKeyPath is
// shown in the footer so users always know where their encryption key lives.
func NewServer(logger *slog.Logger, repo *keys.Repo, masterKeyPath string) (*Server, error) {
	layoutBody, err := templatesFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: read layout: %w", err)
	}

	pages := make(map[string]*template.Template, len(pageContentFiles))
	for name, path := range pageContentFiles {
		contentBody, err := templatesFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("web: read %s: %w", path, err)
		}
		tpl := template.New(name).Funcs(template.FuncMap{"now": time.Now})
		if _, err := tpl.Parse(string(layoutBody)); err != nil {
			return nil, fmt.Errorf("web: parse layout for %s: %w", name, err)
		}
		if _, err := tpl.Parse(string(contentBody)); err != nil {
			return nil, fmt.Errorf("web: parse content for %s: %w", name, err)
		}
		pages[name] = tpl
	}

	partials := template.New("partials").Funcs(template.FuncMap{"now": time.Now})
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
		keys:          repo,
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
		http.Redirect(w, r, "/keys", http.StatusFound)
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
		r.Get("/{id}/edit", s.handleKeysEditDialog)
		r.Put("/{id}", s.handleKeysUpdate)
		r.Delete("/{id}", s.handleKeysDelete)
	})

	return r
}

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
	// Returning empty content replaces the dialog container with nothing.
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

// redirect handles both the htmx and the plain-form fallback case. htmx
// inspects the HX-Redirect header to perform a client-side redirect; ordinary
// browsers see a 303.
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

// pageData is the common shape passed to full-page renders.
type pageData struct {
	Title         string
	Active        string
	Version       string
	MasterKeyPath string
	Keys          []keys.Key
	Flash         string
}

// dialogData is passed to the keys_form partial.
type dialogData struct {
	Editing bool
	Key     keys.Key
	Error   string
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
