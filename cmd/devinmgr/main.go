// Command devinmgr runs the local Devin API-key manager dashboard. The binary
// is meant to be launched on a single user's machine; it listens on localhost,
// stores its data alongside the executable, and exposes an HTMX-driven web UI
// for managing keys, tasks, and sessions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/artifacts"
	"github.com/ggwpgoend/devin-key-manager/internal/config"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

// shutdownGrace is how long the main loop waits for background goroutines
// (poller, checker, reactivator) to drain their current iteration after
// SIGTERM/SIGINT before forcing the process to exit. Five seconds is more
// than enough for an in-flight Devin poll to complete on a healthy
// network and not so long that a hung HTTP call wedges the whole binary.
const shutdownGrace = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()
	logger.Info("starting", "version", version.Version, "addr", cfg.Addr, "db", cfg.DBPath, "artifacts", cfg.ArtifactsDir)

	if err := os.MkdirAll(cfg.ArtifactsDir, 0o755); err != nil {
		return fmt.Errorf("ensure artifacts dir: %w", err)
	}

	cipher, err := crypto.LoadOrCreateCipher(cfg.MasterKeyPath)
	if err != nil {
		return fmt.Errorf("init cipher: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	keysRepo := keys.NewRepo(db, cipher)
	tasksRepo := tasks.NewRepo(db)
	sessionsRepo := sessions.NewRepo(db)
	handoffsRepo := handoffs.NewRepo(db)
	artifactsRepo := artifacts.NewRepo(db)

	// The downloader needs a BearerProvider. We create the manager first
	// with a nil downloader, then wire the downloader after the manager
	// exists so we can call mgr.BearerForSession.
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, handoffsRepo, manager.Options{
		Logger:    logger,
		Artifacts: artifactsRepo,
	})
	downloader := artifacts.NewDownloader(artifactsRepo, cfg.ArtifactsDir, mgr.BearerForSession, logger)
	mgr.SetDownloader(downloader)

	// Background loops are tracked in a WaitGroup so shutdown waits for
	// each loop to observe the cancelled context and exit its current
	// iteration before main() returns. Without this, SIGTERM would tear
	// down the process while a Devin poll or a SQLite write might still
	// be mid-statement, occasionally corrupting the WAL or leaving an
	// in-flight HTTP request half-written.
	var bgWG sync.WaitGroup

	poller := manager.NewPoller(mgr, sessionsRepo, logger, manager.PollerOptions{})
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		if err := poller.Run(ctx); err != nil {
			logger.Warn("poller exited", "err", err)
		}
	}()

	checker := manager.NewChecker(mgr, logger, manager.CheckerOptions{RunOnStart: true})
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		if err := checker.Run(ctx); err != nil {
			logger.Warn("checker exited", "err", err)
		}
	}()

	reactivator := manager.NewReactivator(mgr, logger, manager.ReactivatorOptions{RunOnStart: true})
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		if err := reactivator.Run(ctx); err != nil {
			logger.Warn("reactivator exited", "err", err)
		}
	}()

	srv, err := web.NewServer(logger, web.Deps{
		Keys:      keysRepo,
		Tasks:     tasksRepo,
		Sessions:  sessionsRepo,
		Handoffs:  handoffsRepo,
		Artifacts: artifactsRepo,
		Manager:   mgr,
	}, cfg.MasterKeyPath)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}

	srvErr := srv.Run(ctx, cfg.Addr)

	// After the HTTP server exits the context is cancelled (either by
	// signal or because of an HTTP error). Wait for the background loops
	// up to shutdownGrace before returning.
	done := make(chan struct{})
	go func() { bgWG.Wait(); close(done) }()
	select {
	case <-done:
		logger.Info("shutdown: background loops stopped cleanly")
	case <-time.After(shutdownGrace):
		logger.Warn("shutdown: timed out waiting for background loops", "grace", shutdownGrace)
	}

	if srvErr != nil && !errors.Is(srvErr, context.Canceled) {
		return fmt.Errorf("http: %w", srvErr)
	}
	logger.Info("shutdown complete")
	return nil
}
