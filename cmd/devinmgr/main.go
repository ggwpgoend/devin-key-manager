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
	"syscall"

	"github.com/ggwpgoend/devin-key-manager/internal/config"
	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
	"github.com/ggwpgoend/devin-key-manager/internal/web"
)

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
	mgr := manager.New(keysRepo, tasksRepo, sessionsRepo, manager.Options{Logger: logger})

	poller := manager.NewPoller(mgr, sessionsRepo, logger, manager.PollerOptions{})
	go func() {
		if err := poller.Run(ctx); err != nil {
			logger.Warn("poller exited", "err", err)
		}
	}()

	checker := manager.NewChecker(mgr, logger, manager.CheckerOptions{RunOnStart: true})
	go func() {
		if err := checker.Run(ctx); err != nil {
			logger.Warn("checker exited", "err", err)
		}
	}()

	srv, err := web.NewServer(logger, web.Deps{
		Keys:     keysRepo,
		Tasks:    tasksRepo,
		Sessions: sessionsRepo,
		Manager:  mgr,
	}, cfg.MasterKeyPath)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}

	if err := srv.Run(ctx, cfg.Addr); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("http: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
