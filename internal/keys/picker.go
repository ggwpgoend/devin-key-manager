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
// successful API request so subsequent Pick calls round-robin away.
func (r *Repo) MarkUsed(ctx context.Context, id string) error {
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx,
		`UPDATE keys SET last_used_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("keys: mark used: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
