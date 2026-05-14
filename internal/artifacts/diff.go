package artifacts

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// PR-18 / roadmap D34: diff between artifact versions.
//
// When Devin iterates on a file (sends main.go, then a v2 of main.go,
// then v3), the UI ought to surface the diffs so the user can see
// what changed at each step. This package provides:
//
//   - VersionsByName: find all ready artifacts in a session that share
//     a filename, ordered oldest-first.
//   - Diff: unified diff (3 lines of context) between the raw bytes of
//     two artifacts. Pure Go LCS-based, no external dependencies.
//
// The diff implementation is deliberately small: a Myers diff would
// produce nicer hunks for very large files, but a Longest Common
// Subsequence approach is straightforward, deterministic, and good
// enough for files that fit comfortably in memory (which is the
// typical Devin artifact: source code, configs, JSON).

// VersionGroup is a slice of artifacts with the same filename in the
// same session, ordered oldest-first. The first element is the
// original version.
type VersionGroup struct {
	Filename  string
	Artifacts []Artifact
}

// VersionsByName returns groups of artifacts that share the same
// filename within a single session. Only ready artifacts are
// considered. Groups with a single artifact (i.e. no other version
// to diff against) are excluded.
func (r *Repo) VersionsByName(ctx context.Context, sessionID string) ([]VersionGroup, error) {
	list, err := r.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	by := map[string][]Artifact{}
	for _, a := range list {
		if a.Status != StatusReady || a.Filename == "" {
			continue
		}
		by[a.Filename] = append(by[a.Filename], a)
	}
	var out []VersionGroup
	for fn, arts := range by {
		if len(arts) < 2 {
			continue
		}
		// ListBySession is already ordered by created_at ASC.
		out = append(out, VersionGroup{Filename: fn, Artifacts: arts})
	}
	return out, nil
}

// Diff produces a unified diff between the on-disk contents of two
// artifacts. The output looks like:
//
//   --- a/main.go (artifactID-1)
//   +++ b/main.go (artifactID-2)
//   @@ -1,3 +1,3 @@
//    line one
//   -line two
//   +line two changed
//    line three
//
// Binary files (anything containing a NUL byte in the first 1 KB) are
// reported with a placeholder line rather than diffed.
func (r *Repo) Diff(ctx context.Context, leftID, rightID string) (string, error) {
	a, err := r.Get(ctx, leftID)
	if err != nil {
		return "", err
	}
	b, err := r.Get(ctx, rightID)
	if err != nil {
		return "", err
	}
	if a.LocalPath == "" || b.LocalPath == "" {
		return "", fmt.Errorf("artifacts: diff: missing local path")
	}
	left, err := os.ReadFile(a.LocalPath)
	if err != nil {
		return "", fmt.Errorf("artifacts: diff read left: %w", err)
	}
	right, err := os.ReadFile(b.LocalPath)
	if err != nil {
		return "", fmt.Errorf("artifacts: diff read right: %w", err)
	}
	if isBinary(left) || isBinary(right) {
		return fmt.Sprintf("Binary files differ (left=%d bytes, right=%d bytes)\n", len(left), len(right)), nil
	}
	leftLines := strings.Split(string(left), "\n")
	rightLines := strings.Split(string(right), "\n")
	header := fmt.Sprintf("--- a/%s (%s)\n+++ b/%s (%s)\n", a.Filename, a.ID, b.Filename, b.ID)
	body := unifiedDiff(leftLines, rightLines, 3)
	if body == "" {
		return header + "(files are identical)\n", nil
	}
	return header + body, nil
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// unifiedDiff produces a unified diff body (without file headers).
// `context` is the number of unchanged lines on either side of each
// hunk. Pure LCS — O(n*m) memory and time. Good enough for typical
// artifact sizes.
func unifiedDiff(a, b []string, context int) string {
	ops := lcsOps(a, b)
	if len(ops) == 0 {
		return ""
	}
	// Build hunks by scanning the ops and grouping changes that are
	// within 2*context lines of each other.
	type hunk struct {
		aStart, bStart int
		aLen, bLen     int
		lines          []string
	}
	var hunks []hunk
	var cur *hunk
	flush := func() {
		if cur != nil {
			hunks = append(hunks, *cur)
			cur = nil
		}
	}
	pendingEqual := []string{}
	aLine, bLine := 1, 1
	for _, op := range ops {
		switch op.kind {
		case opEqual:
			if cur == nil {
				// Buffer up to `context` equal lines — they'll be the
				// leading context of the next hunk if a change appears.
				pendingEqual = append(pendingEqual, " "+op.line)
				if len(pendingEqual) > context {
					pendingEqual = pendingEqual[len(pendingEqual)-context:]
				}
			} else {
				// Trailing context inside an open hunk. After `context`
				// equal lines we either keep collecting (because more
				// changes may follow within 2*context) or close.
				cur.lines = append(cur.lines, " "+op.line)
				cur.aLen++
				cur.bLen++
				// Close hunk when we've added 2*context trailing
				// equals — beyond that there's no relationship.
				trailing := 0
				for i := len(cur.lines) - 1; i >= 0 && strings.HasPrefix(cur.lines[i], " "); i-- {
					trailing++
				}
				if trailing >= 2*context {
					// Trim the excess trailing.
					cur.lines = cur.lines[:len(cur.lines)-(trailing-context)]
					cur.aLen -= trailing - context
					cur.bLen -= trailing - context
					flush()
				}
			}
			aLine++
			bLine++
		case opDel:
			if cur == nil {
				cur = &hunk{
					aStart: aLine - len(pendingEqual),
					bStart: bLine - len(pendingEqual),
					aLen:   len(pendingEqual),
					bLen:   len(pendingEqual),
					lines:  append([]string{}, pendingEqual...),
				}
				if cur.aStart < 1 {
					cur.aStart = 1
				}
				if cur.bStart < 1 {
					cur.bStart = 1
				}
				pendingEqual = nil
			}
			cur.lines = append(cur.lines, "-"+op.line)
			cur.aLen++
			aLine++
		case opAdd:
			if cur == nil {
				cur = &hunk{
					aStart: aLine - len(pendingEqual),
					bStart: bLine - len(pendingEqual),
					aLen:   len(pendingEqual),
					bLen:   len(pendingEqual),
					lines:  append([]string{}, pendingEqual...),
				}
				if cur.aStart < 1 {
					cur.aStart = 1
				}
				if cur.bStart < 1 {
					cur.bStart = 1
				}
				pendingEqual = nil
			}
			cur.lines = append(cur.lines, "+"+op.line)
			cur.bLen++
			bLine++
		}
	}
	flush()
	var sb strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.aStart, h.aLen, h.bStart, h.bLen)
		for _, ln := range h.lines {
			sb.WriteString(ln)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

type opKind int

const (
	opEqual opKind = iota
	opAdd
	opDel
)

type diffOp struct {
	kind opKind
	line string
}

// lcsOps returns a list of diff operations that turn a into b, using
// a classic dynamic-programming LCS. The result preserves order:
// equal lines, deletions from a, and additions from b interleaved.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:] and b[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{opDel, a[i]})
			i++
		default:
			ops = append(ops, diffOp{opAdd, b[j]})
			j++
		}
	}
	for i < n {
		ops = append(ops, diffOp{opDel, a[i]})
		i++
	}
	for j < m {
		ops = append(ops, diffOp{opAdd, b[j]})
		j++
	}
	return ops
}
