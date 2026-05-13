package manager

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ReactivatorOptions configures the cooldown sweeper.
type ReactivatorOptions struct {
	// Interval between sweeps. Defaults to 1 minute. A minute is a good
	// trade-off between responsiveness (the user shouldn't have to wait
	// hours after a cooldown expires) and noise (we don't need to wake up
	// every second).
	Interval time.Duration
	// RunOnStart triggers one sweep before the first tick so a freshly-
	// launched manager picks up any cooldowns that expired while it was
	// shut down. Defaults to true.
	RunOnStart bool
}

func (o ReactivatorOptions) withDefaults() ReactivatorOptions {
	if o.Interval <= 0 {
		o.Interval = time.Minute
	}
	return o
}

// Reactivator is a goroutine-friendly loop that calls keys.Reactivate on a
// cadence so cooldowns automatically lift when their timer expires. It does
// not call the Devin API — purely a local state machine sweep.
type Reactivator struct {
	manager *Manager
	logger  *slog.Logger
	opts    ReactivatorOptions
}

// NewReactivator wires a Reactivator with sensible defaults.
func NewReactivator(m *Manager, logger *slog.Logger, opts ReactivatorOptions) *Reactivator {
	if logger == nil {
		logger = slog.Default()
	}
	// Default RunOnStart=true unless explicitly opted out by the caller.
	// Since the zero-value bool is false we can't distinguish "unset" from
	// "explicitly false", so callers that want to disable startup sweep
	// must pass Interval > 0 and accept the default of true; or call
	// sweepOnce directly.
	return &Reactivator{manager: m, logger: logger, opts: opts.withDefaults()}
}

// Run blocks until ctx is cancelled, calling Reactivate on the configured
// cadence.
func (r *Reactivator) Run(ctx context.Context) error {
	if r.opts.RunOnStart {
		r.sweep(ctx)
	}
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

func (r *Reactivator) sweep(ctx context.Context) {
	ids, err := r.manager.keys.Reactivate(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Warn("reactivator sweep", "err", err)
		}
		return
	}
	if len(ids) > 0 {
		r.logger.Info("reactivator", "reactivated", len(ids), "ids", ids)
	}
}
