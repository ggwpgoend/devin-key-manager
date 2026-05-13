package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// dailyCooldown is how long a key is shelved after a fresh quota-exhausted
// signal. After 24 hours the daily ACU budget refills, so the reactivator can
// flip the key back to active.
const dailyCooldown = 24 * time.Hour

// weeklyCooldown is the longer cooldown applied when a key has already used
// up its allotted daily cycles for the current week. The user reported that
// each trial gets two consecutive daily refills before the weekly cap kicks
// in; once that cap is hit the key has to wait until the start of the next
// week. We approximate "next week" as 7 days from the exhaustion event — the
// reactivator only flips keys back to active when the timer is up, so the
// worst case is a slightly longer-than-necessary wait, not a too-short one
// that would burn the user's quota.
const weeklyCooldown = 7 * 24 * time.Hour

// dailyCyclesPerWeek is the number of daily-cooldown cycles a key gets before
// it escalates to a weekly cooldown. The third quota event in a week forces
// the longer wait.
const dailyCyclesPerWeek = 2

// QuotaMark is the public summary of a MarkQuotaExhausted call. It carries
// the new state and cooldown timestamp so callers can log or surface them in
// the UI without re-fetching the row.
type QuotaMark struct {
	KeyID         string
	NewState      State
	CooldownUntil time.Time
	CyclesUsed    int
}

// MarkQuotaExhausted records a runtime quota-exhausted signal for a key. The
// state transitions are:
//
//   - cycles < 2 (0 or 1): cooldown_daily, cooldown_until = now + 24h, cycles++.
//   - cycles >= 2:          cooldown_weekly, cooldown_until = now + 7d,
//     week_reset_at = same, cycles++.
//
// A key already in cooldown_weekly is left in cooldown_weekly with the timer
// extended to now + 7d, on the theory that another quota signal so soon
// means the previous cooldown was lifted too aggressively.
//
// Dead keys are not modified — once a key is marked dead by the validator the
// only way out is manual edit or delete.
func (r *Repo) MarkQuotaExhausted(ctx context.Context, id string) (QuotaMark, error) {
	now := r.now().UTC()
	k, err := r.Get(ctx, id)
	if err != nil {
		return QuotaMark{}, err
	}
	if k.State == StateDead {
		return QuotaMark{}, fmt.Errorf("keys: cannot mark dead key %s as quota-exhausted", id)
	}

	newCycles := k.DailyCyclesUsedThisWeek + 1
	var newState State
	var until time.Time
	var weekReset sql.NullTime

	switch {
	case k.State == StateCooldownWeekly:
		// Already on the weekly bench; just extend the timer.
		newState = StateCooldownWeekly
		until = now.Add(weeklyCooldown)
		weekReset = sql.NullTime{Time: until, Valid: true}
	case newCycles > dailyCyclesPerWeek:
		newState = StateCooldownWeekly
		until = now.Add(weeklyCooldown)
		weekReset = sql.NullTime{Time: until, Valid: true}
	default:
		newState = StateCooldownDaily
		until = now.Add(dailyCooldown)
		// Preserve any existing weekly cap so the counter doesn't lose track
		// of when the cycle window began. If there is none, leave NULL.
		if k.WeekResetAt != nil {
			weekReset = sql.NullTime{Time: k.WeekResetAt.UTC(), Valid: true}
		}
	}

	res, err := r.db.ExecContext(ctx, `UPDATE keys SET
        state = ?,
        cooldown_until = ?,
        daily_cycles_used_this_week = ?,
        week_reset_at = ?,
        updated_at = ?
        WHERE id = ?`,
		string(newState), until, newCycles, weekReset, now, id)
	if err != nil {
		return QuotaMark{}, fmt.Errorf("keys: mark quota exhausted: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return QuotaMark{}, ErrNotFound
	}
	return QuotaMark{
		KeyID:         id,
		NewState:      newState,
		CooldownUntil: until,
		CyclesUsed:    newCycles,
	}, nil
}

// Reactivate flips every key whose cooldown has expired back into the active
// pool. It returns the IDs of the keys it reactivated so callers can log a
// concise summary.
//
// Two reactivation paths:
//
//   - cooldown_daily where cooldown_until <= now: state=active,
//     cooldown_until cleared. Daily-cycle counter is *not* reset — that only
//     happens when the weekly window resets, so the user's "two daily
//     refills, then weekly wait" rule stays enforced across daily cycles.
//   - cooldown_weekly where week_reset_at <= now: state=active,
//     cooldown_until and week_reset_at cleared, daily_cycles_used_this_week
//     zeroed. Effectively a fresh week.
func (r *Repo) Reactivate(ctx context.Context) ([]string, error) {
	now := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("keys: reactivate begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	dailyIDs, err := selectIDs(ctx, tx, `SELECT id FROM keys
        WHERE state = 'cooldown_daily'
          AND cooldown_until IS NOT NULL AND cooldown_until <= ?`, now)
	if err != nil {
		return nil, fmt.Errorf("keys: reactivate daily select: %w", err)
	}
	if len(dailyIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE keys
            SET state = 'active', cooldown_until = NULL, updated_at = ?
            WHERE state = 'cooldown_daily'
              AND cooldown_until IS NOT NULL AND cooldown_until <= ?`, now, now); err != nil {
			return nil, fmt.Errorf("keys: reactivate daily update: %w", err)
		}
	}

	weeklyIDs, err := selectIDs(ctx, tx, `SELECT id FROM keys
        WHERE state = 'cooldown_weekly'
          AND week_reset_at IS NOT NULL AND week_reset_at <= ?`, now)
	if err != nil {
		return nil, fmt.Errorf("keys: reactivate weekly select: %w", err)
	}
	if len(weeklyIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE keys
            SET state = 'active',
                cooldown_until = NULL,
                week_reset_at = NULL,
                daily_cycles_used_this_week = 0,
                updated_at = ?
            WHERE state = 'cooldown_weekly'
              AND week_reset_at IS NOT NULL AND week_reset_at <= ?`, now, now); err != nil {
			return nil, fmt.Errorf("keys: reactivate weekly update: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("keys: reactivate commit: %w", err)
	}
	return append(dailyIDs, weeklyIDs...), nil
}

func selectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CooldownRemaining returns the time remaining on a key's cooldown, or 0 if
// the key is not in a cooldown state. Useful for UI countdown displays.
func (k Key) CooldownRemaining(now time.Time) time.Duration {
	if k.CooldownUntil == nil {
		return 0
	}
	d := k.CooldownUntil.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// ErrInvalidQuotaTransition is returned if MarkQuotaExhausted is called on a
// state that cannot transition (e.g. a dead key).
var ErrInvalidQuotaTransition = errors.New("keys: invalid quota transition")
