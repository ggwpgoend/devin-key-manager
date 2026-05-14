package scheduler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/scheduler"
	"github.com/ggwpgoend/devin-key-manager/internal/schedules"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

type fakeRunner struct {
	mu       sync.Mutex
	fired    []schedules.Schedule
	sessions []string
	err      error
}

func (f *fakeRunner) StartScheduledTask(_ context.Context, sch schedules.Schedule) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fired = append(f.fired, sch)
	if f.err != nil {
		return "", f.err
	}
	id := "devin-fake-" + sch.ID[:6]
	f.sessions = append(f.sessions, id)
	return id, nil
}

func setup(t *testing.T) (*schedules.Repo, *notifications.Repo, *fakeRunner, *scheduler.Scheduler) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schRepo := schedules.NewRepo(db)
	notifRepo := notifications.NewRepo(db)
	runner := &fakeRunner{}
	sch := scheduler.New(schRepo, notifRepo, runner,
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	return schRepo, notifRepo, runner, sch
}

func TestProcessDue_HappyPath(t *testing.T) {
	schRepo, notifRepo, runner, sch := setup(t)
	ctx := context.Background()
	created, err := schRepo.Create(ctx, schedules.CreateInput{
		Title: "ping", Prompt: "do a thing", Kind: schedules.KindInterval,
		IntervalSeconds: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run the loop forward to a time after next_run_at.
	sch.ProcessDue(ctx, time.Now().Add(5*time.Minute))

	if got := len(runner.fired); got != 1 {
		t.Fatalf("runner fired=%d want 1", got)
	}
	if runner.fired[0].ID != created.ID {
		t.Errorf("fired wrong schedule: %s", runner.fired[0].ID)
	}
	// MarkRan should bump next_run_at to the future.
	after, _ := schRepo.Get(ctx, created.ID)
	if after.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at not advanced: %v", after.NextRunAt)
	}
	if after.LastSessionID == "" {
		t.Errorf("expected last_session_id to be set")
	}
	// A notification should have been emitted.
	evs, err := notifRepo.Since(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != notifications.KindScheduleFired {
		t.Errorf("expected one schedule_fired event, got %+v", evs)
	}

	// Idempotent: a second pass immediately should NOT re-fire (next_run is now future).
	sch.ProcessDue(ctx, time.Now())
	if got := len(runner.fired); got != 1 {
		t.Errorf("runner re-fired (count=%d)", got)
	}
}

func TestProcessDue_DisabledSkipped(t *testing.T) {
	schRepo, _, runner, sch := setup(t)
	ctx := context.Background()
	created, err := schRepo.Create(ctx, schedules.CreateInput{
		Title: "off", Prompt: "x", Kind: schedules.KindInterval,
		IntervalSeconds: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := schRepo.SetEnabled(ctx, created.ID, false); err != nil {
		t.Fatal(err)
	}
	sch.ProcessDue(ctx, time.Now().Add(time.Hour))
	if len(runner.fired) != 0 {
		t.Errorf("disabled schedule fired anyway")
	}
}

func TestProcessDue_RunnerError(t *testing.T) {
	schRepo, notifRepo, runner, sch := setup(t)
	ctx := context.Background()
	runner.err = errors.New("no keys available")
	created, err := schRepo.Create(ctx, schedules.CreateInput{
		Title: "errorpath", Prompt: "x", Kind: schedules.KindInterval,
		IntervalSeconds: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sch.ProcessDue(ctx, time.Now().Add(2*time.Minute))
	after, _ := schRepo.Get(ctx, created.ID)
	if after.LastError == "" {
		t.Errorf("expected last_error to be set when runner returns error")
	}
	// Notification still appended so the user knows.
	evs, _ := notifRepo.Since(ctx, 0, 10)
	if len(evs) != 1 {
		t.Errorf("expected 1 notification on error path, got %d", len(evs))
	}
}
