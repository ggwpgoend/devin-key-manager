package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

// bulkDetectConcurrency caps how many DetectPlan calls run in parallel
// during BulkImport. Devin's API has no documented rate limit but a wide
// fan-out from a personal-use box would still look hostile in their
// logs. 8 gives a ~8x speedup for a typical paste of 20 keys without
// risking either client-side socket exhaustion or server-side throttling.
const bulkDetectConcurrency = 8

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
	results := make([]BulkImportResult, len(lines))

	// detection holds the auto-detect outcome for each line. Slots where
	// the line specified a plan or failed earlier are left zero-value.
	type detection struct {
		plan         keys.Plan
		status       devin.ValidateStatus
		unauthorized bool
	}
	detections := make([]detection, len(lines))

	// Stage 1: pre-fill parse-failure rows and figure out which lines
	// need an HTTP detect call. We do this serially because it's cheap
	// and ordering matters for the result slice.
	type detectJob struct {
		idx    int
		apiKey string
	}
	var jobs []detectJob
	for i, ln := range lines {
		results[i] = BulkImportResult{LineNo: ln.LineNo, Label: ln.Label}
		if ln.Error != "" {
			results[i].Outcome = BulkOutcomeParseError
			results[i].Error = ln.Error
			continue
		}
		if ln.APIKey == "" {
			results[i].Outcome = BulkOutcomeBadInput
			results[i].Error = "empty api key"
			continue
		}
		if ln.Plan != "" {
			continue
		}
		jobs = append(jobs, detectJob{idx: i, apiKey: ln.APIKey})
	}

	// Stage 2: parallel DetectPlan across the unknown-plan lines. We
	// fan out up to bulkDetectConcurrency goroutines; the work-stealing
	// channel pattern keeps per-call latency from compounding.
	if len(jobs) > 0 {
		jobCh := make(chan detectJob)
		var wg sync.WaitGroup
		workers := bulkDetectConcurrency
		if workers > len(jobs) {
			workers = len(jobs)
		}
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobCh {
					detected, status, _ := m.DetectPlan(ctx, j.apiKey)
					detections[j.idx] = detection{
						plan:         detected,
						status:       status,
						unauthorized: status == devin.ValidateUnauthorized,
					}
				}
			}()
		}
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
		wg.Wait()
	}

	// Stage 3: serial insert. SQLite's WAL handles concurrent writers,
	// but the Create path also does dedup lookups; we keep this serial
	// to avoid surprising the caller with reordered duplicate handling.
	for i, ln := range lines {
		if results[i].Outcome != "" {
			// Parse/empty failures already populated.
			continue
		}
		plan := ln.Plan
		if plan == "" {
			d := detections[i]
			results[i].DetectedAs = d.plan
			if d.unauthorized {
				results[i].Outcome = BulkOutcomeUnauthorized
				results[i].Error = "unauthorized: " + ln.Label
				continue
			}
			// Network error path: fall through with whatever DetectPlan
			// returned (typically PlanTrial — conservative default).
			plan = d.plan
		}

		k, err := m.keys.Create(ctx, keys.CreateInput{
			Label:  ln.Label,
			Plan:   plan,
			APIKey: ln.APIKey,
		})
		if err != nil {
			if errors.Is(err, keys.ErrDuplicateKey) {
				results[i].Outcome = BulkOutcomeDuplicate
				results[i].Error = "fingerprint already in pool"
			} else {
				results[i].Outcome = BulkOutcomeBadInput
				results[i].Error = err.Error()
			}
			continue
		}
		results[i].Outcome = BulkOutcomeCreated
		results[i].Key = k
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
