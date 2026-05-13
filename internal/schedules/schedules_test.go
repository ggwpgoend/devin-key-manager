package schedules_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func openRepo(t *testing.T) (*schedules.Repo, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return schedules.NewRepo(db), func() { _ = db.Close() }
}

func TestCreateAndGet(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()

	got, err := r.Create(ctx, schedules.CreateInput{
		Title: "daily-report", Prompt: "Generate the daily report.",
		Kind: schedules.KindDaily, DailyHour: 9, DailyMinute: 0, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID == "" {
		t.Fatal("empty ID")
	}
	if got.Title != "daily-report" || got.Kind != schedules.KindDaily {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.Enabled {
		t.Errorf("expected enabled=true")
	}

	fetched, err := r.Get(ctx, got.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.Title != got.Title {
		t.Errorf("get title=%q want %q", fetched.Title, got.Title)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   schedules.CreateInput
		ok   bool
	}{
		{"no title", schedules.CreateInput{Prompt: "x", Kind: schedules.KindInterval, IntervalSeconds: 600}, false},
		{"no prompt", schedules.CreateInput{Title: "x", Kind: schedules.KindInterval, IntervalSeconds: 600}, false},
		{"unknown kind", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.Kind("???")}, false},
		{"interval too short", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindInterval, IntervalSeconds: 5}, false},
		{"daily bad hour", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindDaily, DailyHour: 25}, false},
		{"daily bad minute", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindDaily, DailyHour: 1, DailyMinute: 60}, false},
		{"interval ok", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindInterval, IntervalSeconds: 60}, true},
		{"daily ok", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindDaily, DailyHour: 9, DailyMinute: 30}, true},
		{"bad plan hint", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindDaily, PlanHint: "premium"}, false},
		{"plan trial ok", schedules.CreateInput{Title: "x", Prompt: "x", Kind: schedules.KindDaily, PlanHint: "trial"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.Validate()
			if c.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestComputeNextRun_Interval(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// No anchor → now + interval.
	next := schedules.ComputeNextRun(schedules.KindInterval, 600, 0, 0, now, time.Time{})
	want := now.Add(10 * time.Minute)
	if !next.Equal(want) {
		t.Errorf("interval next=%v want %v", next, want)
	}
	// Recent anchor → anchor + interval.
	anchor := now.Add(-1 * time.Minute)
	next = schedules.ComputeNextRun(schedules.KindInterval, 600, 0, 0, now, anchor)
	want = anchor.Add(10 * time.Minute)
	if !next.Equal(want) {
		t.Errorf("anchored next=%v want %v", next, want)
	}
	// Stale anchor (older than 2× interval) → snap forward from now.
	staleAnchor := now.Add(-2 * time.Hour)
	next = schedules.ComputeNextRun(schedules.KindInterval, 600, 0, 0, now, staleAnchor)
	if next.Before(now) {
		t.Errorf("stale anchor produced past time %v (now=%v)", next, now)
	}
}

func TestComputeNextRun_Daily(t *testing.T) {
	now := time.Date(2026, 1, 1, 8, 0, 0, 0, time.Local)
	// 09:00 hasn't passed yet — should pick today at 09:00.
	next := schedules.ComputeNextRun(schedules.KindDaily, 0, 9, 0, now, time.Time{})
	if next.Local().Hour() != 9 || next.Local().Minute() != 0 {
		t.Errorf("daily next time=%v want 09:00", next.Local())
	}
	if next.Local().Day() != 1 {
		t.Errorf("daily next day=%v want 1", next.Local().Day())
	}
	// 07:00 has already passed — should pick tomorrow at 07:00.
	next = schedules.ComputeNextRun(schedules.KindDaily, 0, 7, 0, now, time.Time{})
	if next.Local().Day() != 2 || next.Local().Hour() != 7 {
		t.Errorf("daily next=%v want day=2 hour=7", next.Local())
	}
}

func TestDueBeforeAndMarkRan(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()

	in := schedules.CreateInput{
		Title: "pulse", Prompt: "ping", Kind: schedules.KindInterval,
		IntervalSeconds: 60, Enabled: true,
	}
	sch, err := r.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// Initial next_run_at is in the future — DueBefore(now) returns empty.
	due, err := r.DueBefore(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("expected no due, got %d", len(due))
	}

	// 5 minutes from now → schedule is due.
	due, err = r.DueBefore(ctx, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != sch.ID {
		t.Errorf("expected 1 due (%s), got %+v", sch.ID, due)
	}

	// MarkRan → next_run_at jumps forward.
	if err := r.MarkRan(ctx, sch.ID, schedules.RunOutcome{SessionID: "devin-abc"}); err != nil {
		t.Fatal(err)
	}
	fetched, _ := r.Get(ctx, sch.ID)
	if !fetched.LastRunAt.Valid {
		t.Errorf("last_run_at not populated")
	}
	if fetched.LastSessionID != "devin-abc" {
		t.Errorf("last_session_id=%q want devin-abc", fetched.LastSessionID)
	}
	if fetched.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at %v should be in the future after MarkRan", fetched.NextRunAt)
	}
}

func TestSetEnabledAndDelete(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()
	sch, err := r.Create(ctx, schedules.CreateInput{
		Title: "x", Prompt: "y", Kind: schedules.KindInterval,
		IntervalSeconds: 600, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetEnabled(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}
	if fetched, _ := r.Get(ctx, sch.ID); fetched.Enabled {
		t.Errorf("expected disabled after SetEnabled(false)")
	}
	if err := r.Delete(ctx, sch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, sch.ID); err != schedules.ErrNotFound {
		t.Errorf("got %v want ErrNotFound after delete", err)
	}
}

func TestList(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()
	for _, title := range []string{"a", "b", "c"} {
		if _, err := r.Create(ctx, schedules.CreateInput{
			Title: title, Prompt: "p", Kind: schedules.KindInterval,
			IntervalSeconds: 600, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("list=%d want 3", len(list))
	}
}
