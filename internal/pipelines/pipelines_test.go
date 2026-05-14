package pipelines_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ggwpgoend/devin-key-manager/internal/pipelines"
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

func TestCreateListGetDelete(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)

	if _, err := repo.Create(ctx, pipelines.CreateInput{Name: ""}); err == nil {
		t.Fatal("expected error on empty name")
	}
	p, err := repo.Create(ctx, pipelines.CreateInput{Name: "demo", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" || p.Version != 1 {
		t.Fatalf("bad pipeline: %+v", p)
	}
	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "demo" || got.Description != "d" {
		t.Fatalf("get returned wrong: %+v", got)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", err, list)
	}
	if err := repo.Delete(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, p.ID); err != pipelines.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestReplaceGraphAndVersionBump(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "p"})

	nodes := []pipelines.NodeInput{
		{ID: "a", Type: pipelines.NodeTrigger, Label: "Start", PosX: 0, PosY: 0},
		{ID: "b", Type: pipelines.NodePrompt, Label: "Ask", PosX: 100, PosY: 0,
			Config: json.RawMessage(`{"prompt":"hello"}`)},
		{ID: "c", Type: pipelines.NodeEnd, Label: "Done", PosX: 200, PosY: 0},
	}
	edges := []pipelines.EdgeInput{
		{SourceID: "a", TargetID: "b", Condition: "default"},
		{SourceID: "b", TargetID: "c", Condition: "default"},
	}
	if err := repo.ReplaceGraph(ctx, p.ID, nodes, edges); err != nil {
		t.Fatalf("replace: %v", err)
	}
	g, err := repo.GetGraph(ctx, p.ID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != 3 || len(g.Edges) != 2 {
		t.Fatalf("expected 3/2, got %d/%d", len(g.Nodes), len(g.Edges))
	}
	if g.Pipeline.Version != 2 {
		t.Fatalf("expected version bump to 2, got %d", g.Pipeline.Version)
	}
	// Replace again with a smaller graph; should not leak old nodes/edges.
	smaller := []pipelines.NodeInput{
		{ID: "x", Type: pipelines.NodeTrigger, Label: "Start", PosX: 0, PosY: 0},
		{ID: "y", Type: pipelines.NodeEnd, Label: "Done", PosX: 100, PosY: 0},
	}
	if err := repo.ReplaceGraph(ctx, p.ID, smaller, []pipelines.EdgeInput{{SourceID: "x", TargetID: "y"}}); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	g2, _ := repo.GetGraph(ctx, p.ID)
	if len(g2.Nodes) != 2 || len(g2.Edges) != 1 {
		t.Fatalf("expected 2/1, got %d/%d", len(g2.Nodes), len(g2.Edges))
	}
}

func TestReplaceGraphRejectsBadEdge(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "p"})
	err := repo.ReplaceGraph(ctx, p.ID,
		[]pipelines.NodeInput{{ID: "a", Type: pipelines.NodeTrigger}},
		[]pipelines.EdgeInput{{SourceID: "a", TargetID: "nope"}},
	)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRunSimulatedLinear(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "linear"})
	_ = repo.ReplaceGraph(ctx, p.ID,
		[]pipelines.NodeInput{
			{ID: "t", Type: pipelines.NodeTrigger},
			{ID: "pmt", Type: pipelines.NodePrompt, Config: json.RawMessage(`{"prompt":"hi"}`)},
			{ID: "e", Type: pipelines.NodeEnd},
		},
		[]pipelines.EdgeInput{
			{SourceID: "t", TargetID: "pmt"},
			{SourceID: "pmt", TargetID: "e"},
		},
	)
	run, err := repo.StartRun(ctx, p.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	exec := pipelines.NewExecutor(repo, nil)
	if err := exec.Run(ctx, run.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := repo.GetRun(ctx, run.ID)
	if got.Status != pipelines.RunSucceeded {
		t.Fatalf("expected succeeded, got %s", got.Status)
	}
	steps, _ := repo.ListSteps(ctx, run.ID)
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}
	if steps[0].Status != pipelines.StepSucceeded {
		t.Fatalf("step0 status: %s", steps[0].Status)
	}
}

func TestRunSimulatedBranch(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "branch"})
	_ = repo.ReplaceGraph(ctx, p.ID,
		[]pipelines.NodeInput{
			{ID: "t", Type: pipelines.NodeTrigger},
			{ID: "c", Type: pipelines.NodeCondition, Config: json.RawMessage(`{"fixture":"true"}`)},
			{ID: "y", Type: pipelines.NodeNotify, Config: json.RawMessage(`{"message":"yes"}`)},
			{ID: "n", Type: pipelines.NodeNotify, Config: json.RawMessage(`{"message":"no"}`)},
			{ID: "e", Type: pipelines.NodeEnd},
		},
		[]pipelines.EdgeInput{
			{SourceID: "t", TargetID: "c"},
			{SourceID: "c", TargetID: "y", Condition: "true"},
			{SourceID: "c", TargetID: "n", Condition: "false"},
			{SourceID: "y", TargetID: "e"},
			{SourceID: "n", TargetID: "e"},
		},
	)
	run, _ := repo.StartRun(ctx, p.ID)
	if err := pipelines.NewExecutor(repo, nil).Run(ctx, run.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	steps, _ := repo.ListSteps(ctx, run.ID)
	// Should be t -> c -> y -> e (NOT n).
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
	// Verify the third step is the "yes" branch.
	var out struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(steps[2].Output, &out)
	if out.Message != "yes" {
		t.Fatalf("took wrong branch, output = %s", string(steps[2].Output))
	}
}

func TestRollbackRunOneLevel(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "rb"})
	_ = repo.ReplaceGraph(ctx, p.ID,
		[]pipelines.NodeInput{
			{ID: "t", Type: pipelines.NodeTrigger},
			{ID: "pmt", Type: pipelines.NodePrompt},
			{ID: "e", Type: pipelines.NodeEnd},
		},
		[]pipelines.EdgeInput{
			{SourceID: "t", TargetID: "pmt"},
			{SourceID: "pmt", TargetID: "e"},
		},
	)
	run, _ := repo.StartRun(ctx, p.ID)
	_ = pipelines.NewExecutor(repo, nil).Run(ctx, run.ID)
	// Walk it back one level: end -> prompt.
	rolled, err := repo.Rollback(ctx, run.ID)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.Status != pipelines.RunRunning {
		t.Fatalf("expected running, got %s", rolled.Status)
	}
	steps, _ := repo.ListSteps(ctx, run.ID)
	// Last step ('e') should now be skipped.
	if steps[len(steps)-1].Status != pipelines.StepSkipped {
		t.Fatalf("last step expected skipped, got %s", steps[len(steps)-1].Status)
	}
}

func TestEmptyPipelineRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := pipelines.NewRepo(db)
	p, _ := repo.Create(ctx, pipelines.CreateInput{Name: "empty"})
	if _, err := repo.StartRun(ctx, p.ID); err != pipelines.ErrEmptyPipeline {
		t.Fatalf("expected ErrEmptyPipeline, got %v", err)
	}
}
