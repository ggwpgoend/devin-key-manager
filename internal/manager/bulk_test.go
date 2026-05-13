package manager_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/crypto"
	"github.com/ggwpgoend/devin-key-manager/internal/devin"
	"github.com/ggwpgoend/devin-key-manager/internal/handoffs"
	"github.com/ggwpgoend/devin-key-manager/internal/keys"
	"github.com/ggwpgoend/devin-key-manager/internal/manager"
	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
	"github.com/ggwpgoend/devin-key-manager/internal/store"
	"github.com/ggwpgoend/devin-key-manager/internal/tasks"
)

// fakeDevin returns 401 for keys containing "bad" and 200 for everything else.
// Quota-exhausted is signalled by the key containing "quota".
func fakeDevin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "bad"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(auth, "quota"):
			w.WriteHeader(http.StatusPaymentRequired)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newManagerForBulkTest(t *testing.T, devinURL string) (*manager.Manager, *keys.Repo) {
	t.Helper()
	dir := t.TempDir()
	c, err := crypto.LoadOrCreateCipher(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keysRepo := keys.NewRepo(db, c)
	mgr := manager.New(
		keysRepo,
		tasks.NewRepo(db),
		sessions.NewRepo(db),
		handoffs.NewRepo(db),
		manager.Options{
			ClientFactory: func(apiKey string) *devin.Client {
				return devin.NewClient(apiKey, devin.WithBaseURL(devinURL))
			},
		},
	)
	return mgr, keysRepo
}

func TestBulkImport_HappyPath(t *testing.T) {
	srv := fakeDevin(t)
	mgr, repo := newManagerForBulkTest(t, srv.URL)

	payload := `
# pasted from notes.txt
trial-1 dev-aaaa-1
trial-2 : dev-aaaa-2
team-1 : paid : dev-aaaa-3
bare-no-label-dev-aaaa-4
`
	results, err := mgr.BulkImport(context.Background(), payload)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	for _, res := range results {
		if res.Outcome != manager.BulkOutcomeCreated {
			t.Errorf("line %d: expected created, got %s (%s)", res.LineNo, res.Outcome, res.Error)
		}
	}
	all, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 keys, got %d", len(all))
	}
	var sawPaid, sawTrialAuto bool
	for _, k := range all {
		if k.Plan == keys.PlanPaid && k.Label == "team-1" {
			sawPaid = true
		}
		if k.Plan == keys.PlanTrial && (k.Label == "trial-1" || k.Label == "trial-2" || strings.HasPrefix(k.Label, "imported-")) {
			sawTrialAuto = true
		}
	}
	if !sawPaid {
		t.Error("expected paid plan to be honoured from explicit input")
	}
	if !sawTrialAuto {
		t.Error("expected auto-detected lines to default to trial")
	}
}

func TestBulkImport_RejectsUnauthorized(t *testing.T) {
	srv := fakeDevin(t)
	mgr, repo := newManagerForBulkTest(t, srv.URL)

	payload := "bad-key dev-bad-1\ngood-key dev-ok-1"
	results, err := mgr.BulkImport(context.Background(), payload)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Outcome != manager.BulkOutcomeUnauthorized {
		t.Errorf("expected unauthorized, got %s", results[0].Outcome)
	}
	if results[1].Outcome != manager.BulkOutcomeCreated {
		t.Errorf("expected created, got %s (%s)", results[1].Outcome, results[1].Error)
	}
	all, _ := repo.List(context.Background())
	if len(all) != 1 {
		t.Errorf("expected only the good key to be inserted, got %d", len(all))
	}
}

func TestBulkImport_DuplicatesSkipped(t *testing.T) {
	srv := fakeDevin(t)
	mgr, _ := newManagerForBulkTest(t, srv.URL)

	payload := "first dev-same\nsecond dev-same"
	results, err := mgr.BulkImport(context.Background(), payload)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if results[0].Outcome != manager.BulkOutcomeCreated {
		t.Errorf("first line: %s", results[0].Outcome)
	}
	if results[1].Outcome != manager.BulkOutcomeDuplicate {
		t.Errorf("second line: %s", results[1].Outcome)
	}
}

func TestDetectPlan_StatusMatrix(t *testing.T) {
	srv := fakeDevin(t)
	mgr, _ := newManagerForBulkTest(t, srv.URL)

	tests := []struct {
		key        string
		wantPlan   keys.Plan
		wantStatus devin.ValidateStatus
		wantErr    bool
	}{
		{key: "dev-ok", wantPlan: keys.PlanTrial, wantStatus: devin.ValidateValid},
		{key: "dev-bad", wantPlan: "", wantStatus: devin.ValidateUnauthorized, wantErr: true},
		{key: "dev-quota", wantPlan: keys.PlanTrial, wantStatus: devin.ValidateQuotaExhausted},
	}
	for _, tc := range tests {
		plan, status, err := mgr.DetectPlan(context.Background(), tc.key)
		if (err != nil) != tc.wantErr {
			t.Errorf("key %q: err=%v, wantErr=%v", tc.key, err, tc.wantErr)
		}
		if plan != tc.wantPlan {
			t.Errorf("key %q: plan=%s, want %s", tc.key, plan, tc.wantPlan)
		}
		if status != tc.wantStatus {
			t.Errorf("key %q: status=%s, want %s", tc.key, status, tc.wantStatus)
		}
	}
}
