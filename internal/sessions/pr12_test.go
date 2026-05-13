package sessions_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

func TestPinAndUnpinMessage(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)
	msg, err := repo.AppendMessage(ctx, sess.ID, sessions.RoleUser, "hello world", time.Now())
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := repo.SetPinned(ctx, msg.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinned, err := repo.PinnedMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("pinned list: %v", err)
	}
	if len(pinned) != 1 || pinned[0].ID != msg.ID {
		t.Fatalf("pinned=%+v", pinned)
	}
	if !pinned[0].Pinned || pinned[0].PinnedAt == nil {
		t.Fatalf("pinned flags lost: %+v", pinned[0])
	}

	if err := repo.SetPinned(ctx, msg.ID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	pinned, _ = repo.PinnedMessages(ctx, sess.ID)
	if len(pinned) != 0 {
		t.Fatalf("expected zero pinned, got %+v", pinned)
	}
}

func TestSearchMessages_Scoped(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)
	_, err := repo.AppendMessage(ctx, sess.ID, sessions.RoleUser, "the quick brown fox jumps over the lazy dog", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.AppendMessage(ctx, sess.ID, sessions.RoleAssistant, "I will help you debug the auth flow", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	hits, err := repo.SearchMessages(ctx, "auth", sess.ID, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d (%+v)", len(hits), hits)
	}
	if !strings.Contains(hits[0].Snippet, "<mark>auth</mark>") {
		t.Fatalf("snippet missing mark: %q", hits[0].Snippet)
	}

	hits, err = repo.SearchMessages(ctx, "missingword", sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %+v", hits)
	}

	// empty query returns nil without error.
	hits, err = repo.SearchMessages(ctx, "  ", sess.ID, 10)
	if err != nil || hits != nil {
		t.Fatalf("empty q: err=%v hits=%v", err, hits)
	}
}

func TestSessionStats(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)
	st, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Total != 1 || st.Open != 1 || st.Closed != 0 {
		t.Fatalf("stats=%+v", st)
	}
	if err := repo.SetStatus(ctx, sess.ID, sessions.StatusCompleted, "test"); err != nil {
		t.Fatal(err)
	}
	st, err = repo.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Open != 0 || st.Closed != 1 {
		t.Fatalf("after close stats=%+v", st)
	}
}

func TestForkAtMessage(t *testing.T) {
	ctx := context.Background()
	repo, sess := newSessionsFixture(t)
	m1, _ := repo.AppendMessage(ctx, sess.ID, sessions.RoleUser, "hello", time.Now())
	m2, _ := repo.AppendMessage(ctx, sess.ID, sessions.RoleAssistant, "hi there", time.Now().Add(time.Second))
	_, _ = repo.AppendMessage(ctx, sess.ID, sessions.RoleUser, "let me try a different path", time.Now().Add(2*time.Second))

	// Fork at m2: new session should have exactly m1 and m2.
	fork, err := repo.Fork(ctx, sessions.ForkInput{SourceID: sess.ID, AnchorMessageID: m2.ID})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork.ForkedFromSessionID != sess.ID || fork.ForkedFromMessageID != m2.ID {
		t.Fatalf("fork lineage missing: %+v", fork)
	}
	if fork.ID == sess.ID {
		t.Fatal("fork must be a new session")
	}
	msgs, err := repo.ListMessages(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d (%+v)", len(msgs), msgs)
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "hi there" {
		t.Fatalf("messages copied wrong: %+v", msgs)
	}
	// Idempotency-ish: original session still has all three.
	orig, err := repo.ListMessages(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != 3 {
		t.Fatalf("original mutated: %+v", orig)
	}
	// Sanity: m1 ID still present on source.
	if orig[0].ID != m1.ID {
		t.Fatalf("source msg ids: %+v", orig)
	}
}
