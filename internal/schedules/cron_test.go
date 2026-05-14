package schedules_test

import (
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
)

func mustParse(t *testing.T, expr string) schedules.Cron {
	t.Helper()
	c, err := schedules.ParseCron(expr)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return c
}

func TestParseCron_Invalid(t *testing.T) {
	bad := []string{
		"",
		"* * * *",     // only 4 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 32 * *",  // dom out of range
		"* * * 13 *",  // month out of range
		"a * * * *",   // junk
		"5-3 * * * *", // inverted
		"*/0 * * * *", // bad step
	}
	for _, b := range bad {
		if _, err := schedules.ParseCron(b); err == nil {
			t.Errorf("expected error for %q", b)
		}
	}
}

func TestCron_Match_EveryMinute(t *testing.T) {
	c := mustParse(t, "* * * * *")
	if !c.Match(time.Date(2025, 1, 1, 12, 30, 0, 0, time.UTC)) {
		t.Error("'* * * * *' should match every minute")
	}
}

func TestCron_Match_Daily(t *testing.T) {
	c := mustParse(t, "0 9 * * *")
	if !c.Match(time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)) {
		t.Error("expected match at 09:00")
	}
	if c.Match(time.Date(2025, 1, 1, 9, 1, 0, 0, time.UTC)) {
		t.Error("expected no match at 09:01")
	}
}

func TestCron_Match_Weekdays(t *testing.T) {
	c := mustParse(t, "0 9 * * 1-5")
	monday := time.Date(2025, 5, 12, 9, 0, 0, 0, time.UTC) // Mon
	sat := time.Date(2025, 5, 17, 9, 0, 0, 0, time.UTC)
	if !c.Match(monday) {
		t.Error("expected Monday match")
	}
	if c.Match(sat) {
		t.Error("expected no Saturday match")
	}
}

func TestCron_Match_StepEveryFifteen(t *testing.T) {
	c := mustParse(t, "*/15 * * * *")
	for _, m := range []int{0, 15, 30, 45} {
		if !c.Match(time.Date(2025, 1, 1, 12, m, 0, 0, time.UTC)) {
			t.Errorf("expected match at minute %d", m)
		}
	}
	if c.Match(time.Date(2025, 1, 1, 12, 7, 0, 0, time.UTC)) {
		t.Error("did not expect match at minute 7")
	}
}

func TestCron_Match_DomOrDowSemantics(t *testing.T) {
	// 13th of the month OR Friday. Both restricted -> OR semantics.
	c := mustParse(t, "0 0 13 * 5")
	// Fri Feb 14 2025 — Friday (matches dow)
	if !c.Match(time.Date(2025, 2, 14, 0, 0, 0, 0, time.UTC)) {
		t.Error("expected Friday match")
	}
	// Sun Mar 13 2025 — 13th (matches dom)... wait, Mar 13 2025 is Thursday.
	// Sun Apr 13 2025 — 13th, Sunday — matches dom not dow but OR rule applies
	if !c.Match(time.Date(2025, 4, 13, 0, 0, 0, 0, time.UTC)) {
		t.Error("expected 13th-of-month match")
	}
	// Non-Friday, non-13th day -> no match
	if c.Match(time.Date(2025, 4, 14, 0, 0, 0, 0, time.UTC)) {
		t.Error("did not expect Apr 14 (Mon) match")
	}
}

func TestCron_Next(t *testing.T) {
	c := mustParse(t, "0 9 * * *")
	from := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	next := c.Next(from)
	want := time.Date(2025, 1, 2, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next: got %s, want %s", next, want)
	}
}

func TestCron_Next_StrictlyAfter(t *testing.T) {
	// Even if `after` matches, Next must return a *later* match.
	c := mustParse(t, "0 9 * * *")
	from := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	next := c.Next(from)
	want := time.Date(2025, 1, 2, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next: got %s, want %s", next, want)
	}
}

func TestCron_SundayBothWays(t *testing.T) {
	c0 := mustParse(t, "0 0 * * 0")
	c7 := mustParse(t, "0 0 * * 7")
	sun := time.Date(2025, 5, 11, 0, 0, 0, 0, time.UTC) // Sunday
	if !c0.Match(sun) || !c7.Match(sun) {
		t.Error("both '0' and '7' should match Sunday")
	}
}
