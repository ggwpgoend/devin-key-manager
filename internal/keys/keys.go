// Package keys implements storage and lifecycle management for Devin API keys.
//
// Keys are persisted in SQLite with their secret value encrypted at rest. A
// SHA-256 fingerprint of the cleartext key is stored alongside the ciphertext
// so callers can detect duplicate keys and refer to them in logs without ever
// touching the plaintext.
package keys

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// Plan describes the subscription tier behind an API key. The string values
// match the CHECK constraint in migrations/0001_init.sql.
type Plan string

const (
	PlanTrial Plan = "trial"
	PlanFree  Plan = "free"
	PlanPaid  Plan = "paid"
)

// Valid returns true if p is one of the recognised plan values.
func (p Plan) Valid() bool {
	switch p {
	case PlanTrial, PlanFree, PlanPaid:
		return true
	default:
		return false
	}
}

// State is the runtime state of a key in the rotation pool.
type State string

const (
	StateActive         State = "active"
	StateCooldownDaily  State = "cooldown_daily"
	StateCooldownWeekly State = "cooldown_weekly"
	StateDead           State = "dead"
)

// Key is the in-memory representation of a row from the keys table. The
// plaintext API key value is only populated by NewKey() and never read back
// from the database — callers that need to call Devin reach for Repo.Reveal.
type Key struct {
	ID                      string
	Label                   string
	Plan                    Plan
	Fingerprint             string
	State                   State
	CooldownUntil           *time.Time
	DailyCyclesUsedThisWeek int
	WeekResetAt             *time.Time
	LastUsedAt              *time.Time
	LastCheckedAt           *time.Time
	LastCheckStatus         string
	LastCheckError          string
	Notes                   string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	// PR-10 metrics. RequestCount is incremented on every successful Devin
	// API call made against this key. LastErrorMessage/LastErrorAt capture
	// the most recent failure for the dashboard. ActivatedAt is set the
	// first time the key is actually used (until then, the key is
	// "installed but never invoked" — useful UX signal). SessionsCountTotal
	// counts the rollup of distinct sessions ever opened on this key.
	RequestCount       int64
	LastErrorMessage   string
	LastErrorAt        *time.Time
	ActivatedAt        *time.Time
	SessionsCountTotal int64
}

// MaskedFingerprint returns a short prefix of the fingerprint suitable for UI
// display (so two similar-looking labels can be told apart at a glance).
func (k Key) MaskedFingerprint() string {
	if len(k.Fingerprint) >= 12 {
		return k.Fingerprint[:12]
	}
	return k.Fingerprint
}

// ErrDuplicateKey is returned when the caller tries to insert a key that is
// already present (matched by fingerprint).
var ErrDuplicateKey = errors.New("keys: api key already exists")

// ErrNotFound is returned when a key with the given ID does not exist.
var ErrNotFound = errors.New("keys: not found")

// Repo manages persistence and encryption for API keys.
type Repo struct {
	db     *store.DB
	cipher *crypto.Cipher
	now    func() time.Time
}

// NewRepo wires a Repo using db for storage and cipher for at-rest encryption.
// The now function is overridable for tests; nil means time.Now.
func NewRepo(db *store.DB, cipher *crypto.Cipher) *Repo {
	return &Repo{db: db, cipher: cipher, now: time.Now}
}

// Fingerprint returns a deterministic identifier for a plaintext API key. The
// value is used to detect duplicates and to reference keys in logs without
// revealing the secret.
func Fingerprint(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

// CreateInput holds the fields callers can specify when inserting a new key.
type CreateInput struct {
	Label  string
	Plan   Plan
	APIKey string
	Notes  string
}

// Create persists a new key, encrypting the API key value with the configured
// cipher. Returns ErrDuplicateKey when the same secret is added twice.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Key, error) {
	if !in.Plan.Valid() {
		return Key{}, fmt.Errorf("keys: invalid plan %q", in.Plan)
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return Key{}, errors.New("keys: label is required")
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" {
		return Key{}, errors.New("keys: api key is required")
	}
	fp := Fingerprint(apiKey)
	encrypted, err := r.cipher.EncryptString(apiKey)
	if err != nil {
		return Key{}, fmt.Errorf("keys: encrypt: %w", err)
	}
	id := uuid.NewString()
	now := r.now().UTC()
	_, err = r.db.ExecContext(ctx, `INSERT INTO keys
        (id, label, plan_type, api_key_encrypted, api_key_fingerprint, state, notes, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		id, label, string(in.Plan), encrypted, fp, in.Notes, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return Key{}, ErrDuplicateKey
		}
		return Key{}, fmt.Errorf("keys: insert: %w", err)
	}
	return Key{
		ID:          id,
		Label:       label,
		Plan:        in.Plan,
		Fingerprint: fp,
		State:       StateActive,
		Notes:       in.Notes,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// List returns all keys ordered by creation time descending.
func (r *Repo) List(ctx context.Context) ([]Key, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+keyColumns+`
        FROM keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("keys: list: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Get loads a single key by ID.
func (r *Repo) Get(ctx context.Context, id string) (Key, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM keys WHERE id = ?`, id)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

// UpdateInput holds the editable subset of fields on a key.
type UpdateInput struct {
	Label string
	Plan  Plan
	Notes string
}

// Update mutates the editable fields of an existing key. The encrypted API key
// is never touched by this call; rotation is a delete+create flow.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) error {
	if !in.Plan.Valid() {
		return fmt.Errorf("keys: invalid plan %q", in.Plan)
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return errors.New("keys: label is required")
	}
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE keys
        SET label = ?, plan_type = ?, notes = ?, updated_at = ?
        WHERE id = ?`, label, string(in.Plan), in.Notes, now, id)
	if err != nil {
		return fmt.Errorf("keys: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a key entirely. Future PRs that link sessions to keys will
// add a soft-delete instead.
func (r *Repo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("keys: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Reveal decrypts and returns the plaintext API key for the row with the given
// ID. Callers must treat the result as sensitive — never log it.
func (r *Repo) Reveal(ctx context.Context, id string) (string, error) {
	var encrypted string
	err := r.db.QueryRowContext(ctx, `SELECT api_key_encrypted FROM keys WHERE id = ?`, id).Scan(&encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keys: reveal: %w", err)
	}
	return r.cipher.DecryptString(encrypted)
}

// keyColumns is the canonical column list used by every SELECT that returns a
// full Key row. Defined once so additions only require one diff.
const keyColumns = `
        id, label, plan_type, api_key_fingerprint, state,
        cooldown_until, daily_cycles_used_this_week, week_reset_at,
        last_used_at, last_checked_at, last_check_status, last_check_error,
        notes, created_at, updated_at,
        request_count, last_error_message, last_error_at, activated_at,
        sessions_count_total`

// rowScanner is implemented by both *sql.Row and *sql.Rows so scanKey can
// service both single and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(s rowScanner) (Key, error) {
	var (
		k           Key
		planStr     string
		stateStr    string
		cooldown    sql.NullTime
		weekReset   sql.NullTime
		lastUsed    sql.NullTime
		lastChecked sql.NullTime
		lastErrAt   sql.NullTime
		activatedAt sql.NullTime
	)
	if err := s.Scan(
		&k.ID, &k.Label, &planStr, &k.Fingerprint, &stateStr,
		&cooldown, &k.DailyCyclesUsedThisWeek, &weekReset,
		&lastUsed, &lastChecked, &k.LastCheckStatus, &k.LastCheckError,
		&k.Notes, &k.CreatedAt, &k.UpdatedAt,
		&k.RequestCount, &k.LastErrorMessage, &lastErrAt, &activatedAt,
		&k.SessionsCountTotal,
	); err != nil {
		return Key{}, fmt.Errorf("keys: scan: %w", err)
	}
	k.Plan = Plan(planStr)
	k.State = State(stateStr)
	if cooldown.Valid {
		t := cooldown.Time
		k.CooldownUntil = &t
	}
	if weekReset.Valid {
		t := weekReset.Time
		k.WeekResetAt = &t
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		k.LastUsedAt = &t
	}
	if lastChecked.Valid {
		t := lastChecked.Time
		k.LastCheckedAt = &t
	}
	if lastErrAt.Valid {
		t := lastErrAt.Time
		k.LastErrorAt = &t
	}
	if activatedAt.Valid {
		t := activatedAt.Time
		k.ActivatedAt = &t
	}
	return k, nil
}

// CheckOutcome bundles the result of a single key validity check.
type CheckOutcome struct {
	// State is the new persisted state for the key. Pass StateActive for a
	// healthy key, StateDead for an unauthorised key, StateCooldownDaily for
	// a key that ran out of quota.
	State State
	// Status is a short, machine-stable tag shown in the UI (e.g. "valid",
	// "unauthorized", "quota_exhausted", "rate_limited", "network_error").
	Status string
	// CooldownUntil overrides the cooldown timestamp; pass nil to clear it.
	CooldownUntil *time.Time
	// Error is the human-readable error message, or empty on success.
	Error string
}

// ApplyCheckOutcome persists the result of a validity check: state, cooldown,
// last_checked_at, last_check_status, last_check_error. Always bumps
// last_checked_at to now() so the UI shows liveness even if the state did not
// change.
func (r *Repo) ApplyCheckOutcome(ctx context.Context, id string, out CheckOutcome) error {
	now := r.now().UTC()
	var cooldown sql.NullTime
	if out.CooldownUntil != nil {
		cooldown = sql.NullTime{Time: out.CooldownUntil.UTC(), Valid: true}
	}
	res, err := r.db.ExecContext(ctx, `UPDATE keys SET
        state = ?, cooldown_until = ?, last_checked_at = ?,
        last_check_status = ?, last_check_error = ?, updated_at = ?
        WHERE id = ?`,
		string(out.State), cooldown, now,
		out.Status, out.Error, now,
		id)
	if err != nil {
		return fmt.Errorf("keys: apply check outcome: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite surfaces unique constraint failures with a message
	// containing "UNIQUE constraint failed". We sniff the string because the
	// driver-specific error types are internal.
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
