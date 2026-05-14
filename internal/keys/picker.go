package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoActiveKey is returned by Pick when no key is currently available.
var ErrNoActiveKey = errors.New("keys: no active key available")

// Pick returns the next key the manager should use, applying:
//
//  1. Plan priority: trial > free > paid. Trials are spent first so they do
//     not expire unused. Paid keys are spent last because they are the most
//     expensive to replace.
//  2. Within a plan, the key with the oldest last_used_at wins (NULLs first),
//     giving a simple round-robin so we don't burn one trial faster than the
//     others.
//
// Only keys whose state is 'active' and whose cooldown has expired (or was
// never set) are considered. Returns ErrNoActiveKey when nothing matches.
func (r *Repo) Pick(ctx context.Context) (Key, error) {
	now := r.now().UTC()
	row := r.db.QueryRowContext(ctx, `SELECT `+keyColumns+`
        FROM keys
        WHERE state = 'active'
          AND (cooldown_until IS NULL OR cooldown_until <= ?)
        ORDER BY CASE plan_type
                   WHEN 'trial' THEN 0
                   WHEN 'free'  THEN 1
                   WHEN 'paid'  THEN 2
                   ELSE 3
                 END ASC,
                 (last_used_at IS NULL) DESC,
                 last_used_at ASC,
                 created_at ASC
        LIMIT 1`, now)
	k, err := scanKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Key{}, ErrNoActiveKey
		}
		return Key{}, err
	}
	return k, nil
}

// MarkUsed stamps last_used_at = now() on the given key. Call this after a
// successful API request so subsequent Pick calls round-robin away. Also
// increments request_count and seeds activated_at on the first use — the
// dashboard surfaces "first activated" as a UX signal.
func (r *Repo) MarkUsed(ctx context.Context, id string) error {
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE keys SET
        last_used_at = ?,
        updated_at = ?,
        request_count = request_count + 1,
        activated_at = COALESCE(activated_at, ?)
        WHERE id = ?`,
		now, now, now, id)
	if err != nil {
		return fmt.Errorf("keys: mark used: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordError captures the most recent failure observed when using this key.
// Non-fatal: errors are logged but do not change key state — that is owned by
// ApplyCheckOutcome / the rotator. Truncates the message to 500 chars.
func (r *Repo) RecordError(ctx context.Context, id, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	now := r.now().UTC()
	_, err := r.db.ExecContext(ctx, `UPDATE keys SET
        last_error_message = ?,
        last_error_at = ?,
        updated_at = ?
        WHERE id = ?`, msg, now, now, id)
	if err != nil {
		return fmt.Errorf("keys: record error: %w", err)
	}
	return nil
}

// BumpSessionsCount increments sessions_count_total. Called from the manager
// each time a new session is opened on this key. Cheap atomic increment.
func (r *Repo) BumpSessionsCount(ctx context.Context, id string) error {
	now := r.now().UTC()
	_, err := r.db.ExecContext(ctx, `UPDATE keys SET
        sessions_count_total = sessions_count_total + 1,
        updated_at = ?
        WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("keys: bump sessions count: %w", err)
	}
	return nil
}
