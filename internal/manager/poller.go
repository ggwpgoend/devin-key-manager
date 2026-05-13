package manager

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

// PollerOptions tunes the background session poller.
type PollerOptions struct {
	// Interval is the cadence at which the poller wakes up and re-syncs every
	// active session. Defaults to 5s when zero.
	Interval time.Duration
	// PerCallTimeout caps individual SyncSession calls so a single hung HTTP
	// request can't block the whole tick. Defaults to 30s when zero.
	PerCallTimeout time.Duration
}

func (o PollerOptions) withDefaults() PollerOptions {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.PerCallTimeout <= 0 {
		o.PerCallTimeout = 30 * time.Second
	}
	return o
}

// Poller is a goroutine-friendly loop that periodically resyncs every active
// session against the Devin Cloud API. It is intentionally simple: no
// per-session goroutines, no priority queue — just a tick loop. PR-3 will
// extend this with quota-detection hooks; PR-4 with artifact downloading.
type Poller struct {
	manager *Manager
	repo    *sessions.Repo
	logger  *slog.Logger
	opts    PollerOptions
}

// NewPoller wires a Poller around a Manager.
func NewPoller(m *Manager, s *sessions.Repo, logger *slog.Logger, opts PollerOptions) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{manager: m, repo: s, logger: logger, opts: opts.withDefaults()}
}

// Run blocks until ctx is done, ticking on opts.Interval. Returns nil on
// graceful shutdown; any unrecoverable error is logged and the loop continues
// (a single bad session must not stop polling for the rest).
func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.opts.Interval)
	defer t.Stop()
	// Run one iteration immediately so the UI doesn't wait for the first tick.
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	active, err := p.repo.ListActive(ctx)
	if err != nil {
		p.logger.Warn("poller list active", "err", err)
		return
	}
	for _, s := range active {
		select {
		case <-ctx.Done():
			return
		default:
		}
		callCtx, cancel := context.WithTimeout(ctx, p.opts.PerCallTimeout)
		err := p.manager.SyncSession(callCtx, s.ID)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, devin.ErrQuotaExhausted):
			p.logger.Info("poller quota exhausted", "session_id", s.ID, "devin_session_id", s.DevinSessionID)
		case errors.Is(err, context.Canceled):
			return
		default:
			p.logger.Warn("poller sync", "session_id", s.ID, "err", err)
		}
	}
}
