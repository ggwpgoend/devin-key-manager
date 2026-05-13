// Package schedules persists recurring task definitions. A small scheduler
// goroutine (see internal/scheduler) wakes up periodically, finds anything
// whose next_run_at has passed, fires it, then bumps next_run_at forward.
//
// Two trigger kinds are supported:
//   - "interval": every InvervalSeconds seconds. Useful for "every 6 hours"
//     style automation.
//   - "daily":    every day at the local DailyHour:DailyMinute.
//
// The package deliberately uses stdlib only — no cron grammar parsing — to
// keep the dependency footprint flat.
package schedules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// Kind enumerates the supported recurrence types.
type Kind string

const (
	KindInterval Kind = "interval"
	KindDaily    Kind = "daily"
)

// Valid reports whether k is a recognised schedule kind.
func (k Kind) Valid() bool {
	switch k {
	case KindInterval, KindDaily:
		return true
	default:
		return false
	}
}

// Schedule is the in-memory representation of a recurring trigger.
type Schedule struct {
	ID              string
	Title           string
	Prompt          string
	PlanHint        string
	Kind            Kind
	IntervalSeconds int64
	DailyHour       int
	DailyMinute     int
	Enabled         bool
	NextRunAt       time.Time
	LastRunAt       sql.NullTime
	LastSessionID   string
	LastError       string
	CreatedAt       time.Time
}

// ErrNotFound is returned by Get when no schedule with the given ID exists.
var ErrNotFound = errors.New("schedules: not found")

// Repo persists Schedule rows.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo wires a Repo on top of the shared store.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// SetNow overrides the time source. Used in tests; passing nil is a no-op.
func (r *Repo) SetNow(fn func() time.Time) {
	if fn != nil {
		r.now = fn
	}
}

// CreateInput holds the user-supplied fields for a new schedule.
type CreateInput struct {
	Title           string
	Prompt          string
	PlanHint        string
	Kind            Kind
	IntervalSeconds int64
	DailyHour       int
	DailyMinute     int
	Enabled         bool
}

// Validate returns an error if the input cannot produce a usable schedule.
func (in CreateInput) Validate() error {
	if strings.TrimSpace(in.Title) == "" {
		return errors.New("title is required")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return errors.New("prompt is required")
	}
	if !in.Kind.Valid() {
		return fmt.Errorf("unknown schedule kind %q", in.Kind)
	}
	switch in.Kind {
	case KindInterval:
		if in.IntervalSeconds < 60 {
			return errors.New("interval must be at least 60 seconds")
		}
	case KindDaily:
		if in.DailyHour < 0 || in.DailyHour > 23 {
			return errors.New("hour must be 0–23")
		}
		if in.DailyMinute < 0 || in.DailyMinute > 59 {
			return errors.New("minute must be 0–59")
		}
	}
	switch strings.TrimSpace(in.PlanHint) {
	case "", "trial", "free", "paid":
	default:
		return fmt.Errorf("plan_hint %q must be one of trial/free/paid (or empty)", in.PlanHint)
	}
	return nil
}

// Create inserts a new schedule, computing the initial NextRunAt from the
// recurrence parameters and the current time.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Schedule, error) {
	if err := in.Validate(); err != nil {
		return Schedule{}, fmt.Errorf("schedules: %w", err)
	}
	id := uuid.NewString()
	now := r.now().UTC()
	next := ComputeNextRun(in.Kind, in.IntervalSeconds, in.DailyHour, in.DailyMinute, now, time.Time{})
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO schedules
		   (id, title, prompt, plan_hint, kind, interval_seconds,
		    daily_hour, daily_minute, enabled, next_run_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, strings.TrimSpace(in.Title), strings.TrimSpace(in.Prompt),
		strings.TrimSpace(in.PlanHint), string(in.Kind), in.IntervalSeconds,
		in.DailyHour, in.DailyMinute, boolToInt(in.Enabled), next, now,
	)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedules: insert: %w", err)
	}
	return r.Get(ctx, id)
}

// Get returns a single schedule by ID.
func (r *Repo) Get(ctx context.Context, id string) (Schedule, error) {
	row := r.db.QueryRowContext(ctx, selectSQL+` WHERE id = ?`, id)
	s, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	return s, err
}

// List returns every schedule ordered by created_at descending.
func (r *Repo) List(ctx context.Context) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx, selectSQL+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("schedules: list: %w", err)
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("schedules: list scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DueBefore returns enabled schedules whose next_run_at has passed.
func (r *Repo) DueBefore(ctx context.Context, t time.Time) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx,
		selectSQL+` WHERE enabled = 1 AND next_run_at <= ? ORDER BY next_run_at ASC`, t.UTC())
	if err != nil {
		return nil, fmt.Errorf("schedules: due: %w", err)
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("schedules: due scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetEnabled flips the enabled flag on a schedule. Disabling does not clear
// the next_run_at so re-enabling does not lose the previous cadence.
func (r *Repo) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE schedules SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	if err != nil {
		return fmt.Errorf("schedules: toggle: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a schedule by ID.
func (r *Repo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("schedules: delete: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// RunOutcome carries the result of a scheduler firing.
type RunOutcome struct {
	SessionID string
	Error     string
}

// MarkRan updates last_run_at, last_session_id, last_error and recomputes
// next_run_at after a firing.
func (r *Repo) MarkRan(ctx context.Context, id string, out RunOutcome) error {
	s, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	now := r.now().UTC()
	next := ComputeNextRun(s.Kind, s.IntervalSeconds, s.DailyHour, s.DailyMinute, now, now)
	_, err = r.db.ExecContext(ctx,
		`UPDATE schedules
		    SET last_run_at = ?, last_session_id = ?, last_error = ?, next_run_at = ?
		  WHERE id = ?`,
		now, out.SessionID, out.Error, next, id,
	)
	if err != nil {
		return fmt.Errorf("schedules: mark ran: %w", err)
	}
	return nil
}

// Claim atomically reserves a schedule for firing by advancing its
// next_run_at to a tentative future value. Returns true only if this call
// won the race against a concurrent run-now or another tick — callers that
// see false should skip firing the schedule. The eventual MarkRan call
// will overwrite next_run_at with the canonical value.
//
// We deliberately tolerate the case where MarkRan never runs (process
// crashes mid-fire): the tentative next_run_at puts the schedule out of
// the DueBefore window, so we don't replay-storm the same task.
func (r *Repo) Claim(ctx context.Context, id string, now time.Time) (bool, error) {
	s, err := r.Get(ctx, id)
	if err != nil {
		return false, err
	}
	tentative := ComputeNextRun(s.Kind, s.IntervalSeconds, s.DailyHour, s.DailyMinute, now, now)
	res, err := r.db.ExecContext(ctx,
		`UPDATE schedules
		    SET next_run_at = ?
		  WHERE id = ?
		    AND enabled = 1
		    AND next_run_at <= ?`,
		tentative, id, now,
	)
	if err != nil {
		return false, fmt.Errorf("schedules: claim: %w", err)
	}
	rows, _ := res.RowsAffected()
	return rows == 1, nil
}

// ComputeNextRun returns the next time a schedule of (kind, params) should
// fire relative to "now" and an optional anchor (last_run_at). For interval
// schedules, ComputeNextRun returns now+IntervalSeconds when no anchor is
// supplied; otherwise anchor+IntervalSeconds — except when anchor is in the
// distant past, in which case we snap forward so a long-paused schedule
// doesn't fire continuously to catch up.
func ComputeNextRun(kind Kind, intervalSeconds int64, dailyHour, dailyMinute int, now, anchor time.Time) time.Time {
	switch kind {
	case KindInterval:
		if intervalSeconds <= 0 {
			intervalSeconds = 60
		}
		if anchor.IsZero() {
			return now.Add(time.Duration(intervalSeconds) * time.Second)
		}
		next := anchor.Add(time.Duration(intervalSeconds) * time.Second)
		if next.Before(now) {
			// Snap forward to avoid replay storms after long downtime.
			return now.Add(time.Duration(intervalSeconds) * time.Second)
		}
		return next
	case KindDaily:
		// Build today's HH:MM in the server's local zone, then jump forward
		// by 24h if it has already passed.
		nowLocal := now.Local()
		todayAt := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(),
			dailyHour, dailyMinute, 0, 0, nowLocal.Location())
		if !todayAt.After(now) {
			todayAt = todayAt.Add(24 * time.Hour)
		}
		return todayAt.UTC()
	}
	return now.Add(time.Hour)
}

const selectSQL = `SELECT id, title, prompt, plan_hint, kind, interval_seconds,
                          daily_hour, daily_minute, enabled, next_run_at,
                          last_run_at, COALESCE(last_session_id, ''),
                          last_error, created_at
                   FROM schedules`

type rowScanner interface {
	Scan(dst ...any) error
}

func scanSchedule(s rowScanner) (Schedule, error) {
	var sch Schedule
	var enabled int
	var kindStr string
	if err := s.Scan(&sch.ID, &sch.Title, &sch.Prompt, &sch.PlanHint, &kindStr,
		&sch.IntervalSeconds, &sch.DailyHour, &sch.DailyMinute, &enabled,
		&sch.NextRunAt, &sch.LastRunAt, &sch.LastSessionID, &sch.LastError,
		&sch.CreatedAt); err != nil {
		return Schedule{}, err
	}
	sch.Kind = Kind(kindStr)
	sch.Enabled = enabled != 0
	return sch, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
