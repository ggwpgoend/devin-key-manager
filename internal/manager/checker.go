package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

// dailyCooldown is the default cooldown applied when the checker finds a key
// in a quota-exhausted state. The real PR-3 quota machine will refine this.
const dailyCooldown = 24 * time.Hour

// CheckResult is the public outcome of a single CheckKey call. The Devin
// validate fields are carried through so the UI can show the raw HTTP status
// and message without re-running the check.
type CheckResult struct {
	KeyID     string
	Label     string
	OldState  keys.State
	NewState  keys.State
	Status    devin.ValidateStatus
	Error     string
	CheckedAt time.Time
}

// CheckKey probes a single key, classifies the response, and persists the
// outcome. Returns the resulting CheckResult and any unexpected internal
// error (DB / decryption / etc.) — validation failures never surface as
// errors, only as Status / Error fields on the CheckResult.
func (m *Manager) CheckKey(ctx context.Context, keyID string) (CheckResult, error) {
	k, err := m.keys.Get(ctx, keyID)
	if err != nil {
		return CheckResult{}, err
	}
	return m.checkOne(ctx, k)
}

// CheckAllKeys runs CheckKey for every key in the pool, sequentially. Returns
// the individual CheckResults in the original key order. An internal error on
// one key is logged and that key is skipped; the loop continues so a single
// bad key cannot block scheduled checks for the others.
func (m *Manager) CheckAllKeys(ctx context.Context) ([]CheckResult, error) {
	all, err := m.keys.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("manager: list keys: %w", err)
	}
	out := make([]CheckResult, 0, len(all))
	for _, k := range all {
		res, err := m.checkOne(ctx, k)
		if err != nil {
			m.logger.Warn("checker skip", "key_id", k.ID, "label", k.Label, "err", err)
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

func (m *Manager) checkOne(ctx context.Context, k keys.Key) (CheckResult, error) {
	plaintext, err := m.keys.Reveal(ctx, k.ID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("manager: reveal key %s: %w", k.ID, err)
	}
	client := m.clientOf(plaintext)
	probe := client.Validate(ctx)
	outcome, newState := mapCheckOutcome(probe, k.State, m.now)
	if err := m.keys.ApplyCheckOutcome(ctx, k.ID, outcome); err != nil {
		return CheckResult{}, fmt.Errorf("manager: apply check outcome for %s: %w", k.ID, err)
	}
	m.logger.Info("checker",
		"key_id", k.ID,
		"label", k.Label,
		"old_state", string(k.State),
		"new_state", string(newState),
		"status", string(probe.Status),
		"http_status", probe.HTTPStatus,
	)
	return CheckResult{
		KeyID:     k.ID,
		Label:     k.Label,
		OldState:  k.State,
		NewState:  newState,
		Status:    probe.Status,
		Error:     probe.Error,
		CheckedAt: m.now().UTC(),
	}, nil
}

// mapCheckOutcome translates a Devin Validate probe into a keys.CheckOutcome
// (which writes to the DB) plus the resulting state (for telemetry / UI).
//
// Rules:
//   - valid             -> state=active, clear cooldown
//   - unauthorized      -> state=dead (key revoked / typo)
//   - quota_exhausted   -> state=cooldown_daily, cooldown_until=now+24h
//   - rate_limited      -> keep existing state, just record status
//   - network_error     -> keep existing state, just record status
//   - api_error         -> keep existing state, just record status
//
// The "keep existing state" branches still bump last_checked_at so the UI
// reflects that the probe ran, but they do not flip a working key into 'dead'
// because the Devin API was temporarily flaky.
func mapCheckOutcome(probe devin.ValidateResult, current keys.State, now func() time.Time) (keys.CheckOutcome, keys.State) {
	out := keys.CheckOutcome{
		State:  current,
		Status: string(probe.Status),
		Error:  probe.Error,
	}
	switch probe.Status {
	case devin.ValidateValid:
		out.State = keys.StateActive
		out.CooldownUntil = nil
		out.Error = ""
	case devin.ValidateUnauthorized:
		out.State = keys.StateDead
		out.CooldownUntil = nil
	case devin.ValidateQuotaExhausted:
		out.State = keys.StateCooldownDaily
		until := now().UTC().Add(dailyCooldown)
		out.CooldownUntil = &until
	default:
		// rate_limited / network_error / api_error: leave state alone.
	}
	return out, out.State
}

// CheckerOptions configures the periodic key checker.
type CheckerOptions struct {
	// Interval between sweeps. Defaults to 1h.
	Interval time.Duration
	// PerKeyTimeout caps the time spent on any single check. Defaults to 15s.
	PerKeyTimeout time.Duration
	// RunOnStart triggers an immediate sweep before the first tick. Useful so
	// the dashboard shows accurate status without waiting an hour after
	// launch. Defaults to true.
	RunOnStart bool
}

// Checker is a background goroutine that periodically validates every key.
type Checker struct {
	manager *Manager
	logger  *slog.Logger
	opts    CheckerOptions
}

// NewChecker builds a Checker with sensible defaults.
func NewChecker(m *Manager, logger *slog.Logger, opts CheckerOptions) *Checker {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Hour
	}
	if opts.PerKeyTimeout <= 0 {
		opts.PerKeyTimeout = 15 * time.Second
	}
	// RunOnStart defaults to true unless caller explicitly opted out.
	return &Checker{manager: m, logger: logger, opts: opts}
}

// Run blocks until ctx is cancelled, running CheckAllKeys every Interval.
func (c *Checker) Run(ctx context.Context) error {
	if c.opts.RunOnStart {
		c.sweep(ctx)
	}
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

func (c *Checker) sweep(ctx context.Context) {
	results, err := c.manager.CheckAllKeys(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			c.logger.Warn("checker sweep", "err", err)
		}
		return
	}
	c.logger.Info("checker sweep done", "checked", len(results))
}
