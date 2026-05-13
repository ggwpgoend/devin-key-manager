// Package scheduler runs the periodic loop that fires due schedules and
// appends notification events for the browser. It is intentionally tiny —
// just a goroutine that ticks every Period, scans for due rows, and calls
// out to a Runner callback.
package scheduler

import (
	"context"
	"log/slog"
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

// ProcessDue runs one pass of the scheduler synchronously. Exposed for tests
// and for manual triggering from the UI.
func (s *Scheduler) ProcessDue(ctx context.Context, now time.Time) {
	due, err := s.repo.DueBefore(ctx, now)
	if err != nil {
		s.logger.Warn("scheduler: due lookup failed", "err", err)
		return
	}
	for _, sch := range due {
		s.fire(ctx, sch)
	}
}

func (s *Scheduler) fire(ctx context.Context, sch schedules.Schedule) {
	sid, err := s.runner.StartScheduledTask(ctx, sch)
	outcome := schedules.RunOutcome{SessionID: sid}
	if err != nil {
		outcome.Error = err.Error()
		s.logger.Warn("scheduler: firing failed", "schedule", sch.ID, "err", err)
	} else {
		s.logger.Info("scheduler: fired", "schedule", sch.ID, "session", sid)
	}
	if mErr := s.repo.MarkRan(ctx, sch.ID, outcome); mErr != nil {
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
	if _, nErr := s.notifs.Append(ctx, notifications.AppendInput{
		Kind:             notifications.KindScheduleFired,
		Title:            "Scheduled: " + sch.Title,
		Body:             body,
		URL:              url,
		RelatedSessionID: sid,
	}); nErr != nil {
		s.logger.Warn("scheduler: notify failed", "schedule", sch.ID, "err", nErr)
	}
}
