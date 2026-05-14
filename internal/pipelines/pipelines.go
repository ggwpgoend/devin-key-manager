// Package pipelines implements the data layer for the n8n-style pipeline
// editor (PR-13 / roadmap item E43.1).
//
// Concepts
//
//   - Pipeline       — a directed acyclic graph of Nodes connected by Edges.
//     The user composes pipelines visually on a Canvas; the data here is
//     what the canvas serialises to.
//   - Node           — a single action: send a prompt, wait, branch on a
//     condition, trigger a handoff, fire a notification. Each node has an
//     opaque JSON config keyed by its Type.
//   - Edge           — a directed connection between two nodes, optionally
//     labelled with a Condition ("true" / "false" / "default") so the
//     runtime knows which branch to follow out of a Condition node.
//   - Run            — one execution of a pipeline. Runs are versioned by
//     pipeline_version so we can still introspect old runs after the user
//     edits the graph.
//   - Step           — a single visited node within a Run. Steps form a
//     monotonic seq used by the "rollback one level" feature: popping the
//     last step rewinds the run to the previous node.
//
// This file is intentionally pure persistence — the executor lives next
// door so we can swap implementations without disturbing the schema.
package pipelines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ggwpgoend/devin-key-manager/internal/store"
)

// NodeType enumerates the valid node kinds. Adding a new type requires
// (a) extending this list and (b) handling it in the executor.
type NodeType string

const (
	NodeTrigger   NodeType = "trigger"
	NodePrompt    NodeType = "prompt"
	NodeWait      NodeType = "wait"
	NodeCondition NodeType = "condition"
	NodeHandoff   NodeType = "handoff"
	NodeNotify    NodeType = "notify"
	NodeEnd       NodeType = "end"
)

// Valid reports whether t is one of the supported node types.
func (t NodeType) Valid() bool {
	switch t {
	case NodeTrigger, NodePrompt, NodeWait, NodeCondition, NodeHandoff, NodeNotify, NodeEnd:
		return true
	}
	return false
}

// RunStatus enumerates the run-level state machine.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// StepStatus enumerates the per-step state machine.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
)

// Errors returned by Repo methods.
var (
	ErrNotFound      = errors.New("pipelines: not found")
	ErrInvalidNode   = errors.New("pipelines: invalid node")
	ErrInvalidEdge   = errors.New("pipelines: invalid edge")
	ErrEmptyPipeline = errors.New("pipelines: empty pipeline")
)

// Pipeline is the top-level container.
type Pipeline struct {
	ID          string
	Name        string
	Description string
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Node represents one action in a pipeline. Config is the JSON-serialised
// per-type configuration; the executor parses it into a typed struct.
type Node struct {
	ID         string
	PipelineID string
	Type       NodeType
	Label      string
	PosX       float64
	PosY       float64
	Config     json.RawMessage
	CreatedAt  time.Time
}

// Edge connects two nodes and carries a condition label.
type Edge struct {
	ID         string
	PipelineID string
	SourceID   string
	TargetID   string
	Condition  string // "true", "false", "default"
	CreatedAt  time.Time
}

// Run is one execution.
type Run struct {
	ID              string
	PipelineID      string
	PipelineVersion int
	Status          RunStatus
	CurrentNodeID   string
	StartedAt       time.Time
	EndedAt         *time.Time
	ErrorMsg        string
}

// Step is one visited node inside a Run.
type Step struct {
	ID        string
	RunID     string
	NodeID    string
	Seq       int
	Status    StepStatus
	StartedAt time.Time
	EndedAt   *time.Time
	Output    json.RawMessage
	ErrorMsg  string
}

// Graph bundles a pipeline with its nodes and edges. Returned by
// GetGraph so the editor can render in one round-trip.
type Graph struct {
	Pipeline Pipeline
	Nodes    []Node
	Edges    []Edge
}

// Repo is the SQL-backed persistence layer.
type Repo struct {
	db  *store.DB
	now func() time.Time
}

// NewRepo builds a Repo. Pass time.Now for production.
func NewRepo(db *store.DB) *Repo {
	return &Repo{db: db, now: time.Now}
}

// CreateInput is the form-friendly version of Pipeline used by handlers.
type CreateInput struct {
	Name        string
	Description string
}

// Create inserts a new empty pipeline (version=1, no nodes).
func (r *Repo) Create(ctx context.Context, in CreateInput) (Pipeline, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Pipeline{}, errors.New("pipelines: name required")
	}
	id := uuid.NewString()
	now := r.now().UTC()
	p := Pipeline{
		ID: id, Name: name, Description: in.Description, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO pipelines
        (id, name, description, version, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Description, p.Version, p.CreatedAt, p.UpdatedAt); err != nil {
		return Pipeline{}, fmt.Errorf("pipelines: insert: %w", err)
	}
	return p, nil
}

// List returns all pipelines, newest first.
func (r *Repo) List(ctx context.Context) ([]Pipeline, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, description, version, created_at, updated_at
        FROM pipelines ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("pipelines: list: %w", err)
	}
	defer rows.Close()
	var out []Pipeline
	for rows.Next() {
		var p Pipeline
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("pipelines: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns just the Pipeline header row.
func (r *Repo) Get(ctx context.Context, id string) (Pipeline, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, name, description, version, created_at, updated_at
        FROM pipelines WHERE id = ?`, id)
	var p Pipeline
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Pipeline{}, ErrNotFound
		}
		return Pipeline{}, fmt.Errorf("pipelines: get: %w", err)
	}
	return p, nil
}

// GetGraph returns the full graph (pipeline + nodes + edges) in one shot.
func (r *Repo) GetGraph(ctx context.Context, id string) (Graph, error) {
	p, err := r.Get(ctx, id)
	if err != nil {
		return Graph{}, err
	}
	nodes, err := r.ListNodes(ctx, id)
	if err != nil {
		return Graph{}, err
	}
	edges, err := r.ListEdges(ctx, id)
	if err != nil {
		return Graph{}, err
	}
	return Graph{Pipeline: p, Nodes: nodes, Edges: edges}, nil
}

// UpdateHeader changes the editable header fields.
func (r *Repo) UpdateHeader(ctx context.Context, id, name, description string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("pipelines: name required")
	}
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE pipelines SET name = ?, description = ?, updated_at = ?
        WHERE id = ?`, name, description, now, id)
	if err != nil {
		return fmt.Errorf("pipelines: update header: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete drops a pipeline and (via cascading FKs) all its nodes/edges/runs.
func (r *Repo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM pipelines WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("pipelines: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Nodes ---

// ListNodes returns all nodes for a pipeline, ordered by creation time so
// the canvas iterates them deterministically.
func (r *Repo) ListNodes(ctx context.Context, pipelineID string) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, pipeline_id, type, label, pos_x, pos_y, config_json, created_at
        FROM pipeline_nodes WHERE pipeline_id = ? ORDER BY created_at ASC`, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("pipelines: list nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var (
			n       Node
			typ     string
			cfgText string
		)
		if err := rows.Scan(&n.ID, &n.PipelineID, &typ, &n.Label, &n.PosX, &n.PosY, &cfgText, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("pipelines: scan node: %w", err)
		}
		n.Type = NodeType(typ)
		n.Config = json.RawMessage(cfgText)
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeInput is the editor-facing version of a node — id is optional for
// fresh nodes, present for updates.
type NodeInput struct {
	ID     string          `json:"id,omitempty"`
	Type   NodeType        `json:"type"`
	Label  string          `json:"label"`
	PosX   float64         `json:"pos_x"`
	PosY   float64         `json:"pos_y"`
	Config json.RawMessage `json:"config"`
}

// EdgeInput mirrors NodeInput for edges.
type EdgeInput struct {
	ID        string `json:"id,omitempty"`
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	Condition string `json:"condition"`
}

// ReplaceGraph atomically swaps the entire node+edge set for a pipeline.
// The editor's "Save" button calls this with the canvas state. We bump
// the pipeline version after a successful swap so old runs can still
// reference a stable snapshot via pipeline_version.
//
// This is by design destructive — the editor maintains the source of
// truth in the browser, and a successful Save replaces the persisted
// graph with what's on the canvas.
func (r *Repo) ReplaceGraph(ctx context.Context, pipelineID string, nodes []NodeInput, edges []EdgeInput) error {
	// Validate first; otherwise a partial transaction could leave the
	// graph in a broken state if a later edge points at a non-existent
	// node we tried to skip.
	knownIDs := map[string]bool{}
	for _, n := range nodes {
		if !n.Type.Valid() {
			return fmt.Errorf("%w: type %q", ErrInvalidNode, n.Type)
		}
		id := n.ID
		if id == "" {
			id = uuid.NewString()
		}
		knownIDs[id] = true
	}
	for _, e := range edges {
		if !knownIDs[e.SourceID] || !knownIDs[e.TargetID] {
			return fmt.Errorf("%w: edge %s -> %s references unknown node",
				ErrInvalidEdge, e.SourceID, e.TargetID)
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipelines: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_edges WHERE pipeline_id = ?`, pipelineID); err != nil {
		return fmt.Errorf("pipelines: clear edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_nodes WHERE pipeline_id = ?`, pipelineID); err != nil {
		return fmt.Errorf("pipelines: clear nodes: %w", err)
	}

	idMap := map[string]string{}
	now := r.now().UTC()
	for _, n := range nodes {
		id := n.ID
		if id == "" {
			id = uuid.NewString()
		}
		idMap[n.ID] = id
		cfg := n.Config
		if len(cfg) == 0 {
			cfg = json.RawMessage("{}")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_nodes
            (id, pipeline_id, type, label, pos_x, pos_y, config_json, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, pipelineID, string(n.Type), n.Label, n.PosX, n.PosY, string(cfg), now); err != nil {
			return fmt.Errorf("pipelines: insert node: %w", err)
		}
	}
	for _, e := range edges {
		eid := e.ID
		if eid == "" {
			eid = uuid.NewString()
		}
		// If sourceID/targetID match a previous "" id, rewrite via idMap.
		src := idMap[e.SourceID]
		if src == "" {
			src = e.SourceID
		}
		tgt := idMap[e.TargetID]
		if tgt == "" {
			tgt = e.TargetID
		}
		cond := strings.TrimSpace(e.Condition)
		if cond == "" {
			cond = "default"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_edges
            (id, pipeline_id, source_id, target_id, condition, created_at)
            VALUES (?, ?, ?, ?, ?, ?)`,
			eid, pipelineID, src, tgt, cond, now); err != nil {
			return fmt.Errorf("pipelines: insert edge: %w", err)
		}
	}

	// Bump version and updated_at.
	if _, err := tx.ExecContext(ctx, `UPDATE pipelines
        SET version = version + 1, updated_at = ?
        WHERE id = ?`, now, pipelineID); err != nil {
		return fmt.Errorf("pipelines: bump version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipelines: commit: %w", err)
	}
	return nil
}

// ListEdges returns the edges for a pipeline.
func (r *Repo) ListEdges(ctx context.Context, pipelineID string) ([]Edge, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, pipeline_id, source_id, target_id, condition, created_at
        FROM pipeline_edges WHERE pipeline_id = ? ORDER BY created_at ASC`, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("pipelines: list edges: %w", err)
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.ID, &e.PipelineID, &e.SourceID, &e.TargetID, &e.Condition, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("pipelines: scan edge: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Runs ---

// StartRun creates a Run row in 'pending' state. The caller is expected to
// pass this to the executor which will transition through running →
// succeeded/failed.
func (r *Repo) StartRun(ctx context.Context, pipelineID string) (Run, error) {
	p, err := r.Get(ctx, pipelineID)
	if err != nil {
		return Run{}, err
	}
	nodes, err := r.ListNodes(ctx, pipelineID)
	if err != nil {
		return Run{}, err
	}
	if len(nodes) == 0 {
		return Run{}, ErrEmptyPipeline
	}
	now := r.now().UTC()
	run := Run{
		ID:              uuid.NewString(),
		PipelineID:      pipelineID,
		PipelineVersion: p.Version,
		Status:          RunPending,
		StartedAt:       now,
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO pipeline_runs
        (id, pipeline_id, pipeline_version, status, started_at)
        VALUES (?, ?, ?, ?, ?)`,
		run.ID, run.PipelineID, run.PipelineVersion, string(run.Status), run.StartedAt); err != nil {
		return Run{}, fmt.Errorf("pipelines: insert run: %w", err)
	}
	return run, nil
}

// AppendStep records a visited node inside a run. seq is computed
// inside the function so concurrent appenders don't race.
func (r *Repo) AppendStep(ctx context.Context, runID, nodeID string, status StepStatus, output json.RawMessage, errMsg string) (Step, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Step{}, fmt.Errorf("pipelines: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1
        FROM pipeline_run_steps WHERE run_id = ?`, runID).Scan(&nextSeq); err != nil {
		return Step{}, fmt.Errorf("pipelines: next seq: %w", err)
	}
	now := r.now().UTC()
	step := Step{
		ID:        uuid.NewString(),
		RunID:     runID,
		NodeID:    nodeID,
		Seq:       nextSeq,
		Status:    status,
		StartedAt: now,
		Output:    output,
		ErrorMsg:  errMsg,
	}
	if len(step.Output) == 0 {
		step.Output = json.RawMessage("{}")
	}
	terminal := status == StepSucceeded || status == StepFailed || status == StepSkipped
	var endedAt any
	if terminal {
		t := now
		endedAt = t
		step.EndedAt = &t
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_run_steps
        (id, run_id, node_id, seq, status, started_at, ended_at, output_json, error_msg)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.ID, step.RunID, step.NodeID, step.Seq, string(step.Status),
		step.StartedAt, endedAt, string(step.Output), step.ErrorMsg); err != nil {
		return Step{}, fmt.Errorf("pipelines: insert step: %w", err)
	}
	// Update the run pointer to the latest node visited.
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_runs SET current_node_id = ? WHERE id = ?`, nodeID, runID); err != nil {
		return Step{}, fmt.Errorf("pipelines: update run pointer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Step{}, fmt.Errorf("pipelines: commit step: %w", err)
	}
	return step, nil
}

// FinishRun stamps the terminal status. errMsg is empty unless status is
// RunFailed.
func (r *Repo) FinishRun(ctx context.Context, runID string, status RunStatus, errMsg string) error {
	now := r.now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE pipeline_runs SET status = ?, ended_at = ?, error_msg = ?
        WHERE id = ?`, string(status), now, errMsg, runID)
	if err != nil {
		return fmt.Errorf("pipelines: finish run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRuns returns the runs for a pipeline, newest first.
func (r *Repo) ListRuns(ctx context.Context, pipelineID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, pipeline_id, pipeline_version, status,
        COALESCE(current_node_id,''), started_at, ended_at, error_msg
        FROM pipeline_runs WHERE pipeline_id = ?
        ORDER BY started_at DESC LIMIT ?`, pipelineID, limit)
	if err != nil {
		return nil, fmt.Errorf("pipelines: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			run     Run
			status  string
			endedAt sql.NullTime
		)
		if err := rows.Scan(&run.ID, &run.PipelineID, &run.PipelineVersion, &status,
			&run.CurrentNodeID, &run.StartedAt, &endedAt, &run.ErrorMsg); err != nil {
			return nil, fmt.Errorf("pipelines: scan run: %w", err)
		}
		run.Status = RunStatus(status)
		if endedAt.Valid {
			t := endedAt.Time
			run.EndedAt = &t
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// ListSteps returns the visited steps of a run, ordered by seq ascending.
func (r *Repo) ListSteps(ctx context.Context, runID string) ([]Step, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, run_id, node_id, seq, status,
        started_at, ended_at, output_json, error_msg
        FROM pipeline_run_steps WHERE run_id = ?
        ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("pipelines: list steps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var (
			s       Step
			status  string
			endedAt sql.NullTime
			cfgText string
		)
		if err := rows.Scan(&s.ID, &s.RunID, &s.NodeID, &s.Seq, &status,
			&s.StartedAt, &endedAt, &cfgText, &s.ErrorMsg); err != nil {
			return nil, fmt.Errorf("pipelines: scan step: %w", err)
		}
		s.Status = StepStatus(status)
		s.Output = json.RawMessage(cfgText)
		if endedAt.Valid {
			t := endedAt.Time
			s.EndedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Rollback rewinds a run by one step. The last step is marked "skipped"
// and current_node_id is repointed at the previous step's node (or null
// if the run had only one step). The run status is set back to
// RunRunning so the user can change the underlying graph and re-execute
// from that point.
//
// This is the "откатить на уровень ниже" capability from the user's
// requirements — explicitly modelled at the run level so the user can
// experiment without losing the previous attempt's data.
func (r *Repo) Rollback(ctx context.Context, runID string) (Run, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: begin rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		lastStepID string
		seq        int
	)
	row := tx.QueryRowContext(ctx, `SELECT id, seq FROM pipeline_run_steps
        WHERE run_id = ? ORDER BY seq DESC LIMIT 1`, runID)
	if err := row.Scan(&lastStepID, &seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, fmt.Errorf("pipelines: rollback: no steps to roll back")
		}
		return Run{}, fmt.Errorf("pipelines: rollback scan: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_run_steps SET status = 'skipped'
        WHERE id = ?`, lastStepID); err != nil {
		return Run{}, fmt.Errorf("pipelines: rollback mark skipped: %w", err)
	}
	// Find the previous step (seq - 1) and re-point the run there.
	var prevNodeID sql.NullString
	prevRow := tx.QueryRowContext(ctx, `SELECT node_id FROM pipeline_run_steps
        WHERE run_id = ? AND seq < ?
        ORDER BY seq DESC LIMIT 1`, runID, seq)
	if err := prevRow.Scan(&prevNodeID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("pipelines: rollback prev: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_runs
        SET current_node_id = ?, status = 'running', ended_at = NULL, error_msg = ''
        WHERE id = ?`, prevNodeID, runID); err != nil {
		return Run{}, fmt.Errorf("pipelines: rollback run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("pipelines: rollback commit: %w", err)
	}
	return r.GetRun(ctx, runID)
}

// GetRun fetches a run by ID.
func (r *Repo) GetRun(ctx context.Context, id string) (Run, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, pipeline_id, pipeline_version, status,
        COALESCE(current_node_id,''), started_at, ended_at, error_msg
        FROM pipeline_runs WHERE id = ?`, id)
	var (
		run     Run
		status  string
		endedAt sql.NullTime
	)
	if err := row.Scan(&run.ID, &run.PipelineID, &run.PipelineVersion, &status,
		&run.CurrentNodeID, &run.StartedAt, &endedAt, &run.ErrorMsg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, fmt.Errorf("pipelines: get run: %w", err)
	}
	run.Status = RunStatus(status)
	if endedAt.Valid {
		t := endedAt.Time
		run.EndedAt = &t
	}
	return run, nil
}
