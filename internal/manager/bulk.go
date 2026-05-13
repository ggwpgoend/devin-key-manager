package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

// BulkImportOutcome categorises what happened to a single line. Used by the
// UI to colour the result table.
type BulkImportOutcome string

const (
	BulkOutcomeCreated      BulkImportOutcome = "created"
	BulkOutcomeDuplicate    BulkImportOutcome = "duplicate"
	BulkOutcomeUnauthorized BulkImportOutcome = "unauthorized"
	BulkOutcomeUnreachable  BulkImportOutcome = "unreachable"
	BulkOutcomeParseError   BulkImportOutcome = "parse_error"
	BulkOutcomeBadInput     BulkImportOutcome = "bad_input"
)

// BulkImportResult mirrors a single keys.BulkLine after it has been processed.
// Key is populated for successfully created entries; for failures it is the
// zero value and Error explains why.
type BulkImportResult struct {
	LineNo     int
	Label      string
	Outcome    BulkImportOutcome
	DetectedAs keys.Plan
	Key        keys.Key
	Error      string
}

// BulkImport accepts the raw textarea payload, parses it via keys.ParseBulk,
// auto-detects the plan for any line that did not specify one, then inserts
// each valid key. Returns a per-line result so the UI can render a summary.
//
// Behaviour:
//
//   - Lines that fail to parse (missing key, unknown plan) are returned with
//     BulkOutcomeBadInput / BulkOutcomeParseError and not touched.
//   - For lines without a plan we call DetectPlan; if detection comes back
//     unauthorized we mark the line BulkOutcomeUnauthorized and skip insert.
//   - Network errors during detection do not block the import: we record the
//     attempt, default the plan to trial (most conservative), and still try
//     to insert the key. The user can fix the plan later via Edit.
//   - Duplicate fingerprints are surfaced as BulkOutcomeDuplicate so the
//     user can decide whether to merge labels.
//
// BulkImport never returns a Go error for per-line failures — only for
// catastrophic failures (DB / repo errors). All other outcomes live on the
// returned slice.
func (m *Manager) BulkImport(ctx context.Context, payload string) ([]BulkImportResult, error) {
	lines := keys.ParseBulk(payload)
	results := make([]BulkImportResult, 0, len(lines))
	for _, ln := range lines {
		res := BulkImportResult{LineNo: ln.LineNo, Label: ln.Label}
		if ln.Error != "" {
			res.Outcome = BulkOutcomeParseError
			res.Error = ln.Error
			results = append(results, res)
			continue
		}
		if ln.APIKey == "" {
			res.Outcome = BulkOutcomeBadInput
			res.Error = "empty api key"
			results = append(results, res)
			continue
		}

		plan := ln.Plan
		if plan == "" {
			detected, status, _ := m.DetectPlan(ctx, ln.APIKey)
			res.DetectedAs = detected
			switch status {
			case devin.ValidateUnauthorized:
				res.Outcome = BulkOutcomeUnauthorized
				res.Error = "unauthorized: " + ln.Label
				results = append(results, res)
				continue
			case devin.ValidateNetworkError:
				// Couldn't reach Devin — still proceed with default plan
				// (trial) so the user can sort it out later. We mark the
				// outcome unreachable+created if insert succeeds below.
			}
			plan = detected
		}

		k, err := m.keys.Create(ctx, keys.CreateInput{
			Label:  ln.Label,
			Plan:   plan,
			APIKey: ln.APIKey,
		})
		if err != nil {
			if errors.Is(err, keys.ErrDuplicateKey) {
				res.Outcome = BulkOutcomeDuplicate
				res.Error = "fingerprint already in pool"
			} else {
				res.Outcome = BulkOutcomeBadInput
				res.Error = err.Error()
			}
			results = append(results, res)
			continue
		}
		res.Outcome = BulkOutcomeCreated
		res.Key = k
		results = append(results, res)
	}
	return results, nil
}

// DetectPlan probes a freshly supplied API key against the Devin API and
// returns the manager's best guess at its plan, plus the raw validate status
// for callers that want to do their own classification.
//
// The Devin Cloud public API does not (today) expose plan information
// directly via Bearer auth. The conservative behaviour is therefore:
//
//   - Validate the key with the cheapest authenticated call.
//   - If the API answers 2xx, return PlanTrial.  Picker priority is
//     trial > free > paid, so defaulting to trial means new keys are spent
//     first — which is exactly what users want for newly-added trial
//     accounts and is harmless for paid accounts (the user can fix the
//     plan via Edit if they care about ordering).
//   - If the API answers 401/403, return ("", unauthorized, nil) so the
//     caller can refuse to insert the key.
//   - On any other status (rate-limited, network error, etc.), return
//     (PlanTrial, status, error) so the caller can decide whether to retry.
//
// This function never holds the plaintext API key longer than the validate
// call itself.
func (m *Manager) DetectPlan(ctx context.Context, apiKey string) (keys.Plan, devin.ValidateStatus, error) {
	client := m.clientOf(apiKey)
	probe := client.Validate(ctx)
	switch probe.Status {
	case devin.ValidateValid:
		return keys.PlanTrial, probe.Status, nil
	case devin.ValidateUnauthorized:
		return "", probe.Status, fmt.Errorf("manager: detect plan: unauthorized")
	case devin.ValidateQuotaExhausted:
		// Quota exhausted is still a working key — assume trial (the most
		// common reason an authed call comes back 402 is a depleted trial).
		return keys.PlanTrial, probe.Status, nil
	case devin.ValidateRateLimited:
		return keys.PlanTrial, probe.Status, nil
	case devin.ValidateNetworkError:
		return keys.PlanTrial, probe.Status, fmt.Errorf("manager: detect plan: network: %s", probe.Error)
	default:
		return keys.PlanTrial, probe.Status, fmt.Errorf("manager: detect plan: api error: %s", probe.Error)
	}
}
