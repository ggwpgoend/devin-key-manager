package schedules

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PR-17 / roadmap E41: full 5-field cron expressions.
//
// Supports the standard Unix cron grammar:
//
//	minute  hour  day-of-month  month  day-of-week
//
// Each field accepts: "*", a single integer, "N-M" ranges, "N,M,O"
// lists, "*/S" step, and "N-M/S" range-with-step. Day-of-week values:
// 0 = Sunday … 6 = Saturday (also 7 = Sunday for parity with vixie-
// cron). Names like JAN/MON are NOT supported — keeps the parser small
// without losing expressiveness for the user's stated use case.
//
// We intentionally don't depend on github.com/robfig/cron: that pulls
// in 1500+ LOC of grammar handling and timezone machinery we don't
// need for "every weekday at 9am" type schedules.

// Cron is a parsed expression. The zero value is invalid; use ParseCron.
type Cron struct {
	expr    string
	minute  fieldSet
	hour    fieldSet
	dom     fieldSet
	month   fieldSet
	dow     fieldSet
	// domSet/dowSet track whether the user explicitly constrained dom
	// or dow. Standard cron OR-combines them when *both* are restricted
	// ("0 0 13 * 5" = "13th of month OR Friday"). When only one is
	// restricted, the other is treated as "any" — matching real cron.
	domSet bool
	dowSet bool
}

// String returns the original expression for round-tripping.
func (c Cron) String() string { return c.expr }

// fieldSet is a bitset over the field's full range. Range bounds depend
// on the field; we store them so Match() knows how to clamp the input.
type fieldSet struct {
	bits uint64 // up to 60 bits used (minutes 0-59)
	min  int
	max  int
}

func newFieldSet(min, max int) fieldSet {
	return fieldSet{min: min, max: max}
}

func (f *fieldSet) set(v int) {
	if v >= f.min && v <= f.max {
		f.bits |= 1 << uint(v-f.min)
	}
}

func (f fieldSet) has(v int) bool {
	if v < f.min || v > f.max {
		return false
	}
	return f.bits&(1<<uint(v-f.min)) != 0
}

// ParseCron parses a standard 5-field cron expression. Returns a usable
// Cron value or an error explaining which field is malformed.
func ParseCron(expr string) (Cron, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Cron{}, errors.New("cron: empty expression")
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("cron: want 5 fields (m h dom mon dow), got %d", len(parts))
	}
	specs := []struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day-of-month", 1, 31},
		{"month", 1, 12},
		{"day-of-week", 0, 7}, // 7 = Sunday alias
	}
	fields := make([]fieldSet, 5)
	for i, s := range specs {
		fs, err := parseField(parts[i], s.min, s.max)
		if err != nil {
			return Cron{}, fmt.Errorf("cron: %s: %w", s.name, err)
		}
		fields[i] = fs
	}
	// Normalise day-of-week 7 -> 0 by adding 0 to the bitset whenever 7
	// is set, then forcing the field's max back to 6 for matching.
	if fields[4].has(7) {
		fields[4].set(0)
	}
	fields[4].max = 6
	fields[4].bits &= (1 << 7) - 1 // clear bit 7

	return Cron{
		expr:   expr,
		minute: fields[0],
		hour:   fields[1],
		dom:    fields[2],
		month:  fields[3],
		dow:    fields[4],
		domSet: parts[2] != "*",
		dowSet: parts[4] != "*",
	}, nil
}

func parseField(spec string, min, max int) (fieldSet, error) {
	fs := newFieldSet(min, max)
	for _, piece := range strings.Split(spec, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			return fs, errors.New("empty piece")
		}
		step := 1
		if idx := strings.Index(piece, "/"); idx >= 0 {
			s, err := strconv.Atoi(piece[idx+1:])
			if err != nil || s <= 0 {
				return fs, fmt.Errorf("invalid step %q", piece[idx+1:])
			}
			step = s
			piece = piece[:idx]
		}
		lo, hi := min, max
		if piece == "*" {
			// already lo..hi
		} else if strings.Contains(piece, "-") {
			pp := strings.SplitN(piece, "-", 2)
			a, err1 := strconv.Atoi(pp[0])
			b, err2 := strconv.Atoi(pp[1])
			if err1 != nil || err2 != nil {
				return fs, fmt.Errorf("invalid range %q", piece)
			}
			lo, hi = a, b
		} else {
			n, err := strconv.Atoi(piece)
			if err != nil {
				return fs, fmt.Errorf("invalid number %q", piece)
			}
			lo, hi = n, n
		}
		if lo > hi {
			return fs, fmt.Errorf("range %d-%d is inverted", lo, hi)
		}
		for v := lo; v <= hi; v += step {
			fs.set(v)
		}
	}
	if fs.bits == 0 {
		return fs, errors.New("no values match")
	}
	return fs, nil
}

// Match reports whether t (in its own timezone) matches the cron
// expression. Seconds and sub-second precision are ignored.
func (c Cron) Match(t time.Time) bool {
	if !c.minute.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) {
		return false
	}
	// Day handling per real-cron semantics: if BOTH dom and dow are
	// restricted, either must match. Otherwise, the restricted one
	// alone is authoritative.
	dom := c.dom.has(t.Day())
	dow := c.dow.has(int(t.Weekday()))
	switch {
	case c.domSet && c.dowSet:
		return dom || dow
	case c.domSet:
		return dom
	case c.dowSet:
		return dow
	default:
		return true
	}
}

// Next returns the next time strictly after `after` at which the
// expression matches. Returns zero Time if no match is found within
// `searchLimit` minutes (5 years by default) — that only happens for
// pathological inputs like "0 0 30 2 *" (Feb 30).
func (c Cron) Next(after time.Time) time.Time {
	const searchLimit = 5 * 365 * 24 * 60
	t := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < searchLimit; i++ {
		if c.Match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
