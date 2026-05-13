package keys

// Test-only backdoors. The file name suffix `_test.go` keeps these helpers
// out of production builds; they exist so cooldown_test.go can simulate the
// passage of time without sleeping for hours.

import (
	"context"
	"time"
)

// SetCooldownUntilForTest forces cooldown_until on a row. Tests use it to
// backdate a cooldown so the reactivator picks the key up immediately.
func (r *Repo) SetCooldownUntilForTest(ctx context.Context, id string, t time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE keys SET cooldown_until = ? WHERE id = ?`, t.UTC(), id)
	return err
}

// SetWeekResetAtForTest forces week_reset_at on a row.
func (r *Repo) SetWeekResetAtForTest(ctx context.Context, id string, t time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE keys SET week_reset_at = ?, cooldown_until = ? WHERE id = ?`, t.UTC(), t.UTC(), id)
	return err
}
