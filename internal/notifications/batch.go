package notifications

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// PR-16 / roadmap F51: notification batching.
//
// The original Append path writes every event straight to the DB. That's
// fine for one-off events ("session completed") but it spams the user
// when something happens 10 times in a row — e.g., a pipeline that
// emits a `notify` action after every step, or a flaky key that rapid-
// fires quota errors.
//
// Batcher sits in front of Append: it collects events in memory and
// flushes them in one of three cases:
//
//  1. **Quiet period** — `flushAfter` elapsed since the last event. The
//     individual events are written one-by-one as if nothing happened.
//  2. **Burst threshold** — buffer grew past `threshold` items inside
//     the rolling window. A *summary* event is emitted instead of the
//     individual events, with a body listing the suppressed titles.
//  3. **Hard cap** — buffer reached `hardCap` regardless of timing.
//     Same summary behaviour as a burst.
//
// Configuration is intentionally tiny: the user can disable batching by
// setting `threshold = 0`. The defaults follow the user's spec ("если
// за 2 минуты накопилось 5 событий — одна суммарная нотификация").

// BatcherConfig controls Batcher behaviour. The zero value yields safe
// defaults (no batching, no goroutine), so callers must set at least
// `Threshold` for batching to kick in.
type BatcherConfig struct {
	// Threshold is the number of events that triggers a summary flush
	// instead of individual flushes. 0 disables batching.
	Threshold int
	// FlushAfter is the quiet-period before below-threshold buffers are
	// flushed as-is. Defaults to 30 seconds when zero.
	FlushAfter time.Duration
	// HardCap force-flushes regardless of timing; defaults to 50.
	HardCap int
	// Window is the rolling window that resets the threshold counter
	// when the oldest pending event ages out. Defaults to 2 minutes.
	Window time.Duration
}

func (c BatcherConfig) normalized() BatcherConfig {
	out := c
	if out.FlushAfter == 0 {
		out.FlushAfter = 30 * time.Second
	}
	if out.HardCap == 0 {
		out.HardCap = 50
	}
	if out.Window == 0 {
		out.Window = 2 * time.Minute
	}
	return out
}

// Batcher wraps a Repo and exposes the same Append signature but
// defers writes to enable bursts to coalesce. Safe for concurrent use.
type Batcher struct {
	repo *Repo
	cfg  BatcherConfig
	now  func() time.Time

	mu      sync.Mutex
	pending []AppendInput
	// firstAt is the time the first event in the current burst arrived.
	// We use it to enforce the rolling window so a long-lived buffer
	// can't suppress events forever.
	firstAt time.Time
	timer   *time.Timer
}

// NewBatcher constructs a Batcher. The Repo is required; cfg is
// normalised. If `cfg.Threshold` is 0, the returned Batcher passes
// events through to the Repo immediately with no buffering, so it's
// always safe to wrap.
func NewBatcher(repo *Repo, cfg BatcherConfig) *Batcher {
	return &Batcher{
		repo: repo,
		cfg:  cfg.normalized(),
		now:  time.Now,
	}
}

// Append queues an event. When the threshold trips, the buffered events
// are flushed as a single summary; otherwise the underlying Repo.Append
// is called either immediately (batching disabled) or after the quiet
// period expires (batching enabled but threshold not yet hit).
func (b *Batcher) Append(ctx context.Context, in AppendInput) error {
	if b.cfg.Threshold <= 0 {
		_, err := b.repo.Append(ctx, in)
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if len(b.pending) == 0 {
		b.firstAt = now
	}
	b.pending = append(b.pending, in)

	// Hard cap or threshold-with-window: force summary now.
	if len(b.pending) >= b.cfg.HardCap || (len(b.pending) >= b.cfg.Threshold && now.Sub(b.firstAt) <= b.cfg.Window) {
		return b.flushSummary(ctx)
	}

	// Otherwise, schedule a quiet-period flush of the individual events.
	b.scheduleFlushLocked()
	return nil
}

// Flush writes whatever is buffered to the DB immediately. Useful at
// shutdown so we don't lose events. Safe to call when buffer is empty.
func (b *Batcher) Flush(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushIndividualLocked(ctx)
}

// scheduleFlushLocked (re)arms the quiet-period timer.
func (b *Batcher) scheduleFlushLocked() {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.cfg.FlushAfter, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		// Use a background context — by the time the timer fires we
		// have no caller context to inherit.
		_ = b.flushIndividualLocked(context.Background())
	})
}

func (b *Batcher) flushIndividualLocked(ctx context.Context) error {
	if len(b.pending) == 0 {
		return nil
	}
	pending := b.pending
	b.pending = nil
	b.firstAt = time.Time{}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	for _, ev := range pending {
		if _, err := b.repo.Append(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batcher) flushSummary(ctx context.Context) error {
	if len(b.pending) == 0 {
		return nil
	}
	pending := b.pending
	b.pending = nil
	b.firstAt = time.Time{}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	// Build a summary body: first line = count + kinds, then up to 10
	// individual titles. We deliberately don't include URLs in the body
	// because the summary is no longer click-actionable as a single
	// destination — clicking takes the user to the timeline.
	kindCounts := make(map[Kind]int)
	for _, ev := range pending {
		kindCounts[ev.Kind]++
	}
	summary := AppendInput{
		Kind:  KindSystem,
		Title: fmt.Sprintf("%d events in the last %s", len(pending), b.cfg.Window),
		URL:   "/notifications",
	}
	body := summary.Title + "\n"
	for k, n := range kindCounts {
		body += fmt.Sprintf(" • %s × %d\n", k, n)
	}
	cap := 10
	if len(pending) < cap {
		cap = len(pending)
	}
	for i := 0; i < cap; i++ {
		body += " — " + pending[i].Title + "\n"
	}
	if len(pending) > cap {
		body += fmt.Sprintf(" … and %d more\n", len(pending)-cap)
	}
	summary.Body = body
	_, err := b.repo.Append(ctx, summary)
	return err
}
