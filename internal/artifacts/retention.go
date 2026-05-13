package artifacts

import (
	"context"
	"fmt"
	"os"
	"time"
)

// PR-17 / roadmap D40: artifact retention.
//
// Older artifacts pile up over time and the user may want them gone
// to reclaim disk. We expose:
//   - SetPinned(id, bool):  mark / unmark an artifact as pinned.
//   - Prune(maxAge, opts): delete unpinned ready artifacts older than
//     maxAge. Returns counts so the UI can show "deleted 12 files,
//     freed 38 MB". Optionally a DryRun mode lists candidates without
//     touching disk.
//
// The retention sweep is intentionally cheap: one indexed query, then
// per-row delete from disk + DB. No background goroutine — callers
// invoke it on demand (HTTP endpoint or scheduler tick).

// PruneOptions tunes Prune behaviour.
type PruneOptions struct {
	// MaxAge: artifacts whose created_at is older than now() - MaxAge
	// are candidates. Must be > 0.
	MaxAge time.Duration
	// DryRun: when true, return candidates but do not delete anything.
	DryRun bool
	// IncludePinned: when true, pinned artifacts are also candidates.
	// Default false — pinned artifacts are spared.
	IncludePinned bool
}

// PruneResult is the summary returned from Prune.
type PruneResult struct {
	Candidates int
	Deleted    int
	FreedBytes int64
	Errors     []string
}

// SetPinned toggles the pinned flag on an artifact. Pinning protects
// the artifact from automated retention sweeps.
func (r *Repo) SetPinned(ctx context.Context, id string, pinned bool) error {
	flag := 0
	if pinned {
		flag = 1
	}
	res, err := r.db.ExecContext(ctx, `UPDATE artifacts SET pinned = ? WHERE id = ?`, flag, id)
	if err != nil {
		return fmt.Errorf("artifacts: pin: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Prune sweeps stale artifacts. With DryRun=true, no filesystem or
// DB writes happen — useful for "what would be deleted?" preview.
func (r *Repo) Prune(ctx context.Context, opts PruneOptions) (PruneResult, error) {
	var res PruneResult
	if opts.MaxAge <= 0 {
		return res, fmt.Errorf("artifacts: prune: MaxAge must be > 0")
	}
	cutoff := r.now().Add(-opts.MaxAge).UTC()
	q := `SELECT id, COALESCE(local_path, ''), size_bytes
	        FROM artifacts
	       WHERE created_at < ?
	         AND status = 'ready'`
	if !opts.IncludePinned {
		q += ` AND pinned = 0`
	}
	q += ` ORDER BY created_at ASC LIMIT 1000`
	rows, err := r.db.QueryContext(ctx, q, cutoff)
	if err != nil {
		return res, fmt.Errorf("artifacts: prune scan: %w", err)
	}
	defer rows.Close()

	type cand struct {
		id   string
		path string
		size int64
	}
	var batch []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.path, &c.size); err != nil {
			return res, fmt.Errorf("artifacts: prune scan row: %w", err)
		}
		batch = append(batch, c)
	}
	res.Candidates = len(batch)
	if opts.DryRun || len(batch) == 0 {
		return res, nil
	}
	for _, c := range batch {
		if c.path != "" {
			if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", c.id, err))
				continue
			}
		}
		if _, err := r.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, c.id); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", c.id, err))
			continue
		}
		res.Deleted++
		res.FreedBytes += c.size
	}
	return res, nil
}
