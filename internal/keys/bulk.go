package keys

import (
	"strings"
)

// BulkLine is a single parsed line from a bulk-import textarea. Plan is empty
// when the line did not specify one (the caller should auto-detect or apply a
// default before calling Create).
type BulkLine struct {
	// LineNo is 1-based and refers to the original input, so error messages
	// can point the user at the right row in their paste.
	LineNo int
	// Label is the human-readable name. When the input line omitted a label
	// the parser synthesises one in the form "imported-<n>".
	Label string
	// Plan is one of PlanTrial / PlanFree / PlanPaid, or "" when the line did
	// not include an explicit plan and the caller should auto-detect.
	Plan Plan
	// APIKey is the raw secret. Never logged.
	APIKey string
	// Error is set if the line could not be parsed at all (e.g. empty
	// secret). The other fields are still populated when possible so the UI
	// can show the offending row.
	Error string
}

// ParseBulk reads a multi-line textarea into a slice of BulkLines. Lines may
// be in any of these forms (whitespace around tokens is trimmed):
//
//	<api_key>
//	<label> <api_key>
//	<label> : <api_key>
//	<label> : <plan> : <api_key>
//
// Blank lines and lines starting with '#' are skipped silently so users can
// paste annotated dumps. Plans must be one of trial/free/paid; anything else
// is rejected with a per-line error and the offending line is dropped from
// the result. (We could keep the line and fail later but rejecting up-front
// gives a tighter feedback loop in the UI.)
//
// Returned lines preserve the input order. LineNo refers to the original
// input including blank/comment lines, so the caller can show "line 7: …"
// even when only some lines made it through.
func ParseBulk(input string) []BulkLine {
	var out []BulkLine
	imported := 0
	for i, raw := range strings.Split(input, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bl := splitBulkLine(line)
		bl.LineNo = lineNo
		if bl.Label == "" {
			imported++
			bl.Label = "imported-" + intToStr(imported)
		}
		if bl.APIKey == "" {
			bl.Error = "missing api key"
		} else if bl.Plan != "" && !bl.Plan.Valid() {
			bl.Error = "unknown plan " + string(bl.Plan)
		}
		out = append(out, bl)
	}
	return out
}

// splitBulkLine handles the three accepted formats. We deliberately accept
// both ':' and whitespace as separators because users paste from various
// sources (password managers, spreadsheets, plain dumps).
//
// Rules:
//  1. If the line contains ':', split on ':' and trim each token. 2 tokens =
//     label + key; 3 tokens = label + plan + key.
//  2. Else if the line contains whitespace, split on the first whitespace
//     run: token[0] is the label, the rest is the key. (We don't allow a
//     plan in whitespace-separated form because keys can contain '-' and
//     plan tokens are too easy to mis-parse.)
//  3. Else the whole line is the key with no label.
func splitBulkLine(line string) BulkLine {
	if strings.Contains(line, ":") {
		parts := strings.Split(line, ":")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		switch len(parts) {
		case 2:
			return BulkLine{Label: parts[0], APIKey: parts[1]}
		case 3:
			return BulkLine{Label: parts[0], Plan: Plan(strings.ToLower(parts[1])), APIKey: parts[2]}
		default:
			// 4+ tokens: assume label : plan : key-which-contains-colons.
			return BulkLine{
				Label:  parts[0],
				Plan:   Plan(strings.ToLower(parts[1])),
				APIKey: strings.Join(parts[2:], ":"),
			}
		}
	}
	if fields := strings.Fields(line); len(fields) >= 2 {
		return BulkLine{Label: fields[0], APIKey: strings.Join(fields[1:], " ")}
	}
	return BulkLine{APIKey: line}
}

func intToStr(n int) string {
	// strconv.Itoa avoided to keep this file dep-free; n is small.
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
