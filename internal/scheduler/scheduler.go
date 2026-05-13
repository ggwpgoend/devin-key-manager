// Package scheduler runs the periodic loop that fires due schedules and
// appends notification events for the browser. It is intentionally tiny —
// just a goroutine that ticks every Period, scans for due rows, and calls
// out to a Runner callback.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
)

// Runner is the callback invoked when a schedule's next_run_at has passed.
// Implementations should kick off the actual task and return the new
// session's Devin id (or an error).
type Runner interface {
	StartScheduledTask(ctx context.Context, sch schedules.Schedule) (sessionID string, err error)
}

// Scheduler glues schedules.Repo and notifications.Repo together with a
// Runner to actually fire tasks. Construction is via New; the loop starts
// only when Start is called.
type Scheduler struct {
	repo   *schedules.Repo
	notifs *notifications.Repo
	runner Runner
	logger *slog.Logger
	period time.Duration
}

// New constructs a Scheduler. If period is zero a 30-second tick is used.
func New(repo *schedules.Repo, notifs *notifications.Repo, runner Runner, logger *slog.Logger, period time.Duration) *Scheduler {
	if period <= 0 {
		period = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{repo: repo, notifs: notifs, runner: runner, logger: logger, period: period}
}

// Start launches a goroutine that ticks every Period and processes any due
// schedules. The goroutine exits when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	// First pass after a short delay so startup races (DB migration, key
	// load) settle before we begin firing.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.ProcessDue(ctx, time.Now().UTC())
		timer.Reset(s.period)
	}
}

// ProcessDue runs one pass of the scheduler. Each due schedule is first
// atomically claimed via the repo so a concurrent manual run-now (or the
// next tick before the previous one finished) can't double-fire it, then
// dispatched to its own goroutine — multiple due schedules don't queue
// behind each other waiting for slow Devin calls.
//
// We block until all goroutines spawned in this pass have finished (using
// a WaitGroup) so tests can deterministically observe their effects, and
// so the next tick's claim sees the updated state.
func (s *Scheduler) ProcessDue(ctx context.Context, now time.Time) {
	due, err := s.repo.DueBefore(ctx, now)
	if err != nil {
		s.logger.Warn("scheduler: due lookup failed", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, sch := range due {
		// Claim under the loop's ctx — if claim fails we silently skip.
		claimed, cErr := s.repo.Claim(ctx, sch.ID, now)
		if cErr != nil {
			s.logger.Warn("scheduler: claim failed", "schedule", sch.ID, "err", cErr)
			continue
		}
		if !claimed {
			// Someone else (run-now or a concurrent tick) got there first.
			continue
		}
		wg.Add(1)
		go func(sch schedules.Schedule) {
			defer wg.Done()
			s.fire(ctx, sch)
		}(sch)
	}
	wg.Wait()
}

func (s *Scheduler) fire(_ context.Context, sch schedules.Schedule) {
	// Use a detached context for the Devin call so a cancellation from
	// the loop's ctx (process shutdown) doesn't cut off an in-flight
	// session creation halfway through. We keep an overall deadline so
	// truly hung calls don't pin goroutines forever.
	callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sid, err := s.runner.StartScheduledTask(callCtx, sch)
	outcome := schedules.RunOutcome{SessionID: sid}
	if err != nil {
		outcome.Error = err.Error()
		s.logger.Warn("scheduler: firing failed", "schedule", sch.ID, "err", err)
	} else {
		s.logger.Info("scheduler: fired", "schedule", sch.ID, "session", sid)
	}
	// MarkRan + notify run on the detached ctx as well — they must complete
	// even if the originating tick's ctx has since been cancelled, so the
	// schedule's last_run_at reflects reality on the next startup.
	if mErr := s.repo.MarkRan(callCtx, sch.ID, outcome); mErr != nil {
		s.logger.Warn("scheduler: mark ran failed", "schedule", sch.ID, "err", mErr)
	}
	if s.notifs == nil {
		return
	}
	body := sch.Prompt
	url := ""
	if sid != "" {
		url = "/sessions/" + sid
	}
	if err != nil {
		body = err.Error()
	}
	if _, nErr := s.notifs.Append(callCtx, notifications.AppendInput{
		Kind:             notifications.KindScheduleFired,
		Title:            "Scheduled: " + sch.Title,
		Body:             body,
		URL:              url,
		RelatedSessionID: sid,
	}); nErr != nil {
		s.logger.Warn("scheduler: notify failed", "schedule", sch.ID, "err", nErr)
	}
}
