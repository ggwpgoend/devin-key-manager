package notifications_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func mustStore(t *testing.T) *store.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBatcher_Disabled_PassThrough(t *testing.T) {
	db := mustStore(t)
	repo := notifications.NewRepo(db)
	b := notifications.NewBatcher(repo, notifications.BatcherConfig{Threshold: 0})
	for i := 0; i < 3; i++ {
		if err := b.Append(context.Background(), notifications.AppendInput{Title: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := repo.Recent(context.Background(), 50)
	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}
}

func TestBatcher_ThresholdEmitsSummary(t *testing.T) {
	db := mustStore(t)
	repo := notifications.NewRepo(db)
	b := notifications.NewBatcher(repo, notifications.BatcherConfig{
		Threshold:  3,
		FlushAfter: time.Minute, // wide so quiet flush doesn't fire
		Window:     time.Minute,
	})
	for i := 0; i < 3; i++ {
		if err := b.Append(context.Background(), notifications.AppendInput{Title: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := repo.Recent(context.Background(), 50)
	if len(events) != 1 {
		t.Fatalf("expected 1 summary event, got %d", len(events))
	}
	if events[0].Kind != notifications.KindSystem {
		t.Errorf("expected system kind, got %q", events[0].Kind)
	}
}

func TestBatcher_Flush_DrainsBuffer(t *testing.T) {
	db := mustStore(t)
	repo := notifications.NewRepo(db)
	b := notifications.NewBatcher(repo, notifications.BatcherConfig{
		Threshold:  10,            // never trips
		FlushAfter: time.Minute,   // never triggers
		Window:     2 * time.Hour, // long
	})
	for i := 0; i < 2; i++ {
		if err := b.Append(context.Background(), notifications.AppendInput{Title: "y"}); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing should be in DB yet (buffered).
	events, _ := repo.Recent(context.Background(), 50)
	if len(events) != 0 {
		t.Fatalf("expected 0 events while buffered, got %d", len(events))
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ = repo.Recent(context.Background(), 50)
	if len(events) != 2 {
		t.Errorf("after flush, expected 2 events, got %d", len(events))
	}
}
