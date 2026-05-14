package pipelines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Executor walks a Graph step-by-step, recording each visited node as a
// Step. The runtime is intentionally side-effect-light in PR-13: actions
// are simulated and their *intent* is recorded as JSON output on the
// step. Future PRs can wire the executor to manager.SendFollowUp /
// notifier.Push / etc. without changing the Repo or the editor.
//
// Why simulate first?
//   - We want the UI to be usable for design / inspection before any
//     "real" execution can wreck a quota.
//   - Conditional branching needs SOMETHING to evaluate against; the
//     simulated executor returns deterministic outputs so the user can
//     walk through their graph and verify the topology before they wire
//     real prompts.
//
// The executor is goroutine-safe in the sense that one Executor can run
// many pipelines concurrently — each Run is independent.
type Executor struct {
	repo   *Repo
	logger *slog.Logger
	// Action hooks. Each returns an opaque JSON output that is recorded
	// in the step row, and an error if the action failed. Nil hooks fall
	// back to a stub implementation (useful in tests and during the first
	// rollout where we don't want real Devin calls yet).
	PromptHook    func(ctx context.Context, cfg PromptConfig) (json.RawMessage, error)
	WaitHook      func(ctx context.Context, cfg WaitConfig) (json.RawMessage, error)
	ConditionHook func(ctx context.Context, cfg ConditionConfig) (bool, json.RawMessage, error)
	HandoffHook   func(ctx context.Context, cfg HandoffConfig) (json.RawMessage, error)
	NotifyHook    func(ctx context.Context, cfg NotifyConfig) (json.RawMessage, error)
}

// PromptConfig is the typed view of a `prompt` node's config JSON.
type PromptConfig struct {
	Prompt          string `json:"prompt"`
	SessionStrategy string `json:"session_strategy"` // "new" | "current"
}

// WaitConfig is the typed view of a `wait` node's config JSON.
type WaitConfig struct {
	Strategy    string `json:"strategy"`     // "idle" | "duration"
	DurationSec int    `json:"duration_sec"` // for duration strategy
}

// ConditionConfig is the typed view of a `condition` node's config JSON.
// The simplest possible expression: compare an LHS (a path like
// "last_message.content") to a literal RHS using one of {eq, neq,
// contains, regex}. The simulated evaluator always returns a
// deterministic result based on the configured fixture.
type ConditionConfig struct {
	Left     string `json:"left"`     // path / variable name
	Right    string `json:"right"`    // literal
	Operator string `json:"operator"` // eq, neq, contains, regex
	// Fixture lets the user pin a deterministic answer for the simulated
	// executor — useful for stepping through a graph in the UI.
	Fixture string `json:"fixture"` // "true" | "false" | "" (auto)
}

// HandoffConfig is the typed view of a `handoff` node's config JSON.
type HandoffConfig struct {
	Reason string `json:"reason"`
}

// NotifyConfig is the typed view of a `notify` node's config JSON.
type NotifyConfig struct {
	Channel string `json:"channel"` // "browser" | "telegram" (future)
	Message string `json:"message"`
}

// NewExecutor returns an Executor ready to run pipelines.
func NewExecutor(repo *Repo, logger *slog.Logger) *Executor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{repo: repo, logger: logger}
}

// Run executes a pipeline end-to-end. It transitions the run row through
// pending → running → succeeded/failed, and appends one Step per visited
// node. Returns once the runner reaches an End node or runs out of
// outgoing edges. Errors abort the run and stamp RunFailed.
//
// The executor walks the graph in topological order starting from the
// (unique) Trigger node. For a Condition node, the executor evaluates
// the predicate (or honours the Fixture) and follows the edge with the
// matching "true" / "false" label. For non-condition nodes, the
// executor follows the first edge labelled "default" (or the only
// outgoing edge if unlabelled).
func (e *Executor) Run(ctx context.Context, runID string) error {
	run, err := e.repo.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunPending {
		return fmt.Errorf("pipelines: run %s not in pending state (was %s)", runID, run.Status)
	}
	graph, err := e.repo.GetGraph(ctx, run.PipelineID)
	if err != nil {
		return err
	}

	// Index nodes & outgoing edges for cheap lookup.
	nodesByID := make(map[string]Node, len(graph.Nodes))
	for _, n := range graph.Nodes {
		nodesByID[n.ID] = n
	}
	outEdges := make(map[string][]Edge, len(graph.Nodes))
	for _, edge := range graph.Edges {
		outEdges[edge.SourceID] = append(outEdges[edge.SourceID], edge)
	}
	var trigger *Node
	for i := range graph.Nodes {
		if graph.Nodes[i].Type == NodeTrigger {
			trigger = &graph.Nodes[i]
			break
		}
	}
	if trigger == nil {
		_ = e.repo.FinishRun(ctx, runID, RunFailed, "no trigger node")
		return errors.New("pipelines: no trigger node")
	}

	cur := trigger
	maxSteps := 200 // safety cap to avoid infinite loops in user graphs
	for i := 0; i < maxSteps; i++ {
		if err := ctx.Err(); err != nil {
			_ = e.repo.FinishRun(ctx, runID, RunCancelled, err.Error())
			return err
		}
		var (
			out      json.RawMessage
			execErr  error
			condTrue bool
		)
		switch cur.Type {
		case NodeTrigger:
			out = json.RawMessage(`{"note":"trigger fired"}`)
		case NodePrompt:
			cfg, perr := parsePromptCfg(cur.Config)
			if perr != nil {
				execErr = perr
			} else if e.PromptHook != nil {
				out, execErr = e.PromptHook(ctx, cfg)
			} else {
				out = simulatePrompt(cfg)
			}
		case NodeWait:
			cfg, perr := parseWaitCfg(cur.Config)
			if perr != nil {
				execErr = perr
			} else if e.WaitHook != nil {
				out, execErr = e.WaitHook(ctx, cfg)
			} else {
				out = simulateWait(cfg)
			}
		case NodeCondition:
			cfg, perr := parseConditionCfg(cur.Config)
			if perr != nil {
				execErr = perr
			} else if e.ConditionHook != nil {
				condTrue, out, execErr = e.ConditionHook(ctx, cfg)
			} else {
				condTrue, out = simulateCondition(cfg)
			}
		case NodeHandoff:
			cfg, perr := parseHandoffCfg(cur.Config)
			if perr != nil {
				execErr = perr
			} else if e.HandoffHook != nil {
				out, execErr = e.HandoffHook(ctx, cfg)
			} else {
				out = simulateHandoff(cfg)
			}
		case NodeNotify:
			cfg, perr := parseNotifyCfg(cur.Config)
			if perr != nil {
				execErr = perr
			} else if e.NotifyHook != nil {
				out, execErr = e.NotifyHook(ctx, cfg)
			} else {
				out = simulateNotify(cfg)
			}
		case NodeEnd:
			out = json.RawMessage(`{"note":"end reached"}`)
		default:
			execErr = fmt.Errorf("unknown node type %q", cur.Type)
		}

		stepStatus := StepSucceeded
		errMsg := ""
		if execErr != nil {
			stepStatus = StepFailed
			errMsg = execErr.Error()
		}
		if _, err := e.repo.AppendStep(ctx, runID, cur.ID, stepStatus, out, errMsg); err != nil {
			e.logger.Warn("append step failed", "err", err)
		}
		if execErr != nil {
			_ = e.repo.FinishRun(ctx, runID, RunFailed, execErr.Error())
			return execErr
		}
		if cur.Type == NodeEnd {
			_ = e.repo.FinishRun(ctx, runID, RunSucceeded, "")
			return nil
		}
		// Pick the next edge.
		next, picked := pickNextEdge(outEdges[cur.ID], cur.Type, condTrue)
		if !picked {
			// No more edges — treat as a clean finish.
			_ = e.repo.FinishRun(ctx, runID, RunSucceeded, "")
			return nil
		}
		nextNode, ok := nodesByID[next.TargetID]
		if !ok {
			err := fmt.Errorf("pipelines: dangling edge to %s", next.TargetID)
			_ = e.repo.FinishRun(ctx, runID, RunFailed, err.Error())
			return err
		}
		cur = &nextNode
	}
	err = fmt.Errorf("pipelines: max steps (%d) exceeded", maxSteps)
	_ = e.repo.FinishRun(ctx, runID, RunFailed, err.Error())
	return err
}

// pickNextEdge picks the outgoing edge to follow based on node type and
// (for conditions) the boolean result. Edges with condition='default'
// match anything; for conditions we specifically look for 'true'/'false'.
func pickNextEdge(edges []Edge, nodeType NodeType, condTrue bool) (Edge, bool) {
	if nodeType == NodeCondition {
		want := "false"
		if condTrue {
			want = "true"
		}
		for _, e := range edges {
			if e.Condition == want {
				return e, true
			}
		}
		// Fallback: a single default edge.
		for _, e := range edges {
			if e.Condition == "default" {
				return e, true
			}
		}
		return Edge{}, false
	}
	for _, e := range edges {
		if e.Condition == "default" {
			return e, true
		}
	}
	// If no explicit default, take the first edge.
	if len(edges) > 0 {
		return edges[0], true
	}
	return Edge{}, false
}

// --- config parsers ---

func parsePromptCfg(b json.RawMessage) (PromptConfig, error) {
	var c PromptConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("prompt config: %w", err)
	}
	return c, nil
}

func parseWaitCfg(b json.RawMessage) (WaitConfig, error) {
	var c WaitConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("wait config: %w", err)
	}
	return c, nil
}

func parseConditionCfg(b json.RawMessage) (ConditionConfig, error) {
	var c ConditionConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("condition config: %w", err)
	}
	return c, nil
}

func parseHandoffCfg(b json.RawMessage) (HandoffConfig, error) {
	var c HandoffConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("handoff config: %w", err)
	}
	return c, nil
}

func parseNotifyCfg(b json.RawMessage) (NotifyConfig, error) {
	var c NotifyConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("notify config: %w", err)
	}
	return c, nil
}

// --- simulators ---

func simulatePrompt(c PromptConfig) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"simulated":        true,
		"prompt":           c.Prompt,
		"session_strategy": c.SessionStrategy,
		"note":             "no real Devin call was made (simulated mode)",
	})
	return b
}

func simulateWait(c WaitConfig) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"simulated":    true,
		"strategy":     c.Strategy,
		"duration_sec": c.DurationSec,
		"slept_at":     time.Now().UTC().Format(time.RFC3339),
	})
	return b
}

func simulateCondition(c ConditionConfig) (bool, json.RawMessage) {
	// Honour the fixture when set.
	var result bool
	switch strings.ToLower(strings.TrimSpace(c.Fixture)) {
	case "true":
		result = true
	case "false":
		result = false
	default:
		// Auto: a deterministic, useful default — if the LHS contains the
		// substring "ok" the condition is true. This way the user can
		// step through their graph with realistic-feeling results
		// without wiring anything.
		result = strings.Contains(strings.ToLower(c.Left+" "+c.Right), "ok")
	}
	b, _ := json.Marshal(map[string]any{
		"simulated": true,
		"result":    result,
		"left":      c.Left,
		"right":     c.Right,
		"operator":  c.Operator,
		"fixture":   c.Fixture,
	})
	return result, b
}

func simulateHandoff(c HandoffConfig) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"simulated": true,
		"reason":    c.Reason,
		"note":      "would rotate to next available key",
	})
	return b
}

func simulateNotify(c NotifyConfig) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"simulated": true,
		"channel":   c.Channel,
		"message":   c.Message,
	})
	return b
}
