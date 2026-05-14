package observability_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ggwpgoend/devin-key-manager/internal/observability"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, tmp)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seed(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	// 2 keys, 1 task, 2 sessions, 1 handoff between them, some messages.
	stmts := []string{
		`INSERT INTO keys (id, label, plan_type, api_key_encrypted, api_key_fingerprint, state, request_count, last_used_at, created_at, updated_at)
		 VALUES ('k1', 'Devin trial', 'trial', '', 'fp1', 'active', 42, '2024-01-01 12:00:00', '2024-01-01 00:00:00', '2024-01-01 00:00:00')`,
		`INSERT INTO keys (id, label, plan_type, api_key_encrypted, api_key_fingerprint, state, request_count, last_used_at, created_at, updated_at)
		 VALUES ('k2', 'Devin free', 'free', '', 'fp2', 'active', 11, '2024-01-01 12:00:00', '2024-01-01 00:00:00', '2024-01-01 00:00:00')`,
		`INSERT INTO tasks (id, title, initial_prompt, status, created_at, updated_at)
		 VALUES ('t1', 'Refactor auth', 'do it', 'running', datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
		`INSERT INTO sessions (id, task_id, key_id, status, started_at)
		 VALUES ('s1', 't1', 'k1', 'completed', datetime('now', '-50 minutes'))`,
		`INSERT INTO sessions (id, task_id, key_id, status, started_at)
		 VALUES ('s2', 't1', 'k2', 'running', datetime('now', '-20 minutes'))`,
		`INSERT INTO handoffs (id, task_id, from_session_id, to_session_id, markdown, created_at)
		 VALUES ('h1', 't1', 's1', 's2', '# summary', datetime('now', '-25 minutes'))`,
		`INSERT INTO messages (id, session_id, role, content, ts) VALUES ('m1','s1','user','hi',datetime('now', '-40 minutes'))`,
		`INSERT INTO messages (id, session_id, role, content, ts) VALUES ('m2','s1','assistant','hey',datetime('now', '-39 minutes'))`,
		`INSERT INTO messages (id, session_id, role, content, ts) VALUES ('m3','s2','user','continue',datetime('now', '-15 minutes'))`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v: %s", err, s)
		}
	}
}

func TestTimeSeriesQueries(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seed(t, db)
	repo := observability.NewRepo(db)

	got, err := repo.SessionsStarted(ctx, observability.BucketHour, 24*time.Hour)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(got.Points) == 0 {
		t.Fatal("expected at least one session bucket")
	}
	if got.Points[0].Count <= 0 {
		t.Fatalf("bucket count: %d", got.Points[0].Count)
	}

	msgs, err := repo.MessagesSent(ctx, observability.BucketHour, 24*time.Hour)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	total := 0
	for _, p := range msgs.Points {
		total += p.Count
	}
	if total != 3 {
		t.Fatalf("messages total = %d, want 3", total)
	}

	hand, err := repo.HandoffsCreated(ctx, observability.BucketHour, 24*time.Hour)
	if err != nil {
		t.Fatalf("handoffs: %v", err)
	}
	total = 0
	for _, p := range hand.Points {
		total += p.Count
	}
	if total != 1 {
		t.Fatalf("handoffs total = %d, want 1", total)
	}
}

func TestSessionStateBreakdown(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seed(t, db)
	repo := observability.NewRepo(db)

	bd, err := repo.SessionStateBreakdown(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if bd["completed"] != 1 || bd["running"] != 1 {
		t.Fatalf("breakdown: %+v", bd)
	}
}

func TestKeyUsageTop(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seed(t, db)
	repo := observability.NewRepo(db)

	rows, err := repo.KeyUsageTop(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].Requests < rows[1].Requests {
		t.Fatal("not sorted descending")
	}
	if rows[0].Label == "" {
		t.Fatal("expected label populated")
	}
}

func TestSessionGraph(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seed(t, db)
	repo := observability.NewRepo(db)

	g, err := repo.SessionGraphForTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Title != "Refactor auth" {
		t.Fatalf("title: %q", g.Title)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes: %d", len(g.Nodes))
	}
	if len(g.Edges) != 1 {
		t.Fatalf("edges: %d", len(g.Edges))
	}
	if g.Edges[0].From != "s1" || g.Edges[0].To != "s2" {
		t.Fatalf("edge: %+v", g.Edges[0])
	}
}

func TestSessionGraphMissingTask(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := observability.NewRepo(db)
	if _, err := repo.SessionGraphForTask(ctx, "ghost"); err == nil {
		t.Fatal("expected error for missing task")
	}
}
