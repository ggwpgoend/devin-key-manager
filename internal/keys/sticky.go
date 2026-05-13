package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PR-17 / roadmap A9: sticky session-to-key.
//
// When a task already has handoff history, we'd rather keep using the
// same key for new sessions in that task so Devin doesn't lose context
// (different keys = different accounts in some plans).
//
// PickWithPreference takes an optional `preferKeyID`. If that key is
// still active and not on cooldown, it's returned unchanged. Otherwise
// we fall back to the normal Pick() round-robin so the caller never
// blocks.
//
// The "preferred" key is supplied by the caller — typically the manager
// derives it from the last successful session of the same task. Picker
// itself stays state-free, which makes it easy to reason about.

// PickWithPreference returns preferKeyID when it's usable, else falls
// back to Pick. An empty preferKeyID short-circuits to plain Pick.
func (r *Repo) PickWithPreference(ctx context.Context, preferKeyID string) (Key, error) {
	if preferKeyID == "" {
		return r.Pick(ctx)
	}
	now := r.now().UTC()
	row := r.db.QueryRowContext(ctx, `SELECT `+keyColumns+`
        FROM keys
        WHERE id = ?
          AND state = 'active'
          AND (cooldown_until IS NULL OR cooldown_until <= ?)`,
		preferKeyID, now)
	k, err := scanKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r.Pick(ctx)
		}
		return Key{}, fmt.Errorf("keys: pick preferred: %w", err)
	}
	return k, nil
}
