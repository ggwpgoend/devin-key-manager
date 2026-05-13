package notifications_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/notifications"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func openRepo(t *testing.T) (*notifications.Repo, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return notifications.NewRepo(db), func() { _ = db.Close() }
}

func TestAppendAndSince(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()

	id1, err := r.Append(ctx, notifications.AppendInput{
		Kind:  notifications.KindDevinMessage,
		Title: "Devin replied", Body: "Hi!",
		URL: "/sessions/x", RelatedSessionID: "x",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	id2, err := r.Append(ctx, notifications.AppendInput{
		Kind: notifications.KindScheduleFired, Title: "Daily report fired",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id1 {
		t.Errorf("expected monotonic ids: id1=%d id2=%d", id1, id2)
	}

	all, err := r.Since(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("since 0 = %d want 2", len(all))
	}
	if all[0].Kind != notifications.KindDevinMessage || all[1].Kind != notifications.KindScheduleFired {
		t.Errorf("ordering wrong: %+v", all)
	}
	if all[0].RelatedSessionID != "x" {
		t.Errorf("related session lost: %q", all[0].RelatedSessionID)
	}

	// Querying since id1 returns only id2.
	after, err := r.Since(ctx, id1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != id2 {
		t.Errorf("since id1 = %+v want only id2=%d", after, id2)
	}
}

func TestAppendDefaultsAndRecent(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := r.Append(ctx, notifications.AppendInput{Title: "without kind"}); err != nil {
		t.Fatal(err)
	}
	recent, err := r.Recent(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Kind != notifications.KindSystem {
		t.Errorf("default kind lost: %+v", recent)
	}
}

func TestAppendRejectsEmptyTitle(t *testing.T) {
	r, cleanup := openRepo(t)
	defer cleanup()
	if _, err := r.Append(context.Background(), notifications.AppendInput{Title: ""}); err == nil {
		t.Errorf("expected error for empty title")
	}
}
