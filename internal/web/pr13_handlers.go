package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/pipelines"
	"github.com/ggwpgoend/devin-key-manager/internal/version"
)

// PR-13 (E43.1) HTTP layer for the pipeline editor. Handlers serve two
// audiences:
//
//   - The HTML editor page renders the canvas via a server-side template
//     that bootstraps with the latest graph JSON. After that, all reads
//     and writes go through the JSON endpoints below — the canvas is a
//     thin client that PUT-s its full state on Save.
//
//   - Future automations (CLI, browser extensions) can use the same JSON
//     endpoints to script pipelines without touching the HTML side.

type pipelinesIndexRow struct {
	Pipeline  pipelines.Pipeline
	NodeCount int
}

func (s *Server) handlePipelinesIndex(w http.ResponseWriter, r *http.Request) {
	all, err := s.pipelines.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]pipelinesIndexRow, 0, len(all))
	for _, p := range all {
		nodes, _ := s.pipelines.ListNodes(r.Context(), p.ID)
		rows = append(rows, pipelinesIndexRow{Pipeline: p, NodeCount: len(nodes)})
	}
	s.renderPage(w, r, "pipelines_index", pageData{
		Title:         "Pipelines",
		Active:        "pipelines",
		NavCurrent:    "pipelines",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Flash:         r.URL.Query().Get("flash"),
		Pipelines:     rows,
	})
}

func (s *Server) handlePipelinesCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := s.pipelines.Create(r.Context(), pipelines.CreateInput{
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
	})
	if err != nil {
		if isHTMX(r) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.redirect(w, r, "/pipelines?flash="+urlEncode("could not create: "+err.Error()))
		return
	}
	s.redirect(w, r, "/pipelines/"+p.ID)
}



func (s *Server) handlePipelineEditor(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	g, err := s.pipelines.GetGraph(r.Context(), id)
	if err != nil {
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	runs, _ := s.pipelines.ListRuns(r.Context(), id, 20)
	graphBytes, _ := json.Marshal(serializeGraph(g))
	runsBytes, _ := json.Marshal(serializeRuns(runs))
	s.renderPage(w, r, "pipeline_editor", pageData{
		Title:         g.Pipeline.Name,
		Active:        "pipelines",
		NavCurrent:    "pipelines",
		Version:       version.Version,
		MasterKeyPath: s.masterKeyPath,
		Flash:         r.URL.Query().Get("flash"),
		PipelineRow:   g.Pipeline,
		GraphJSON:     string(graphBytes),
		RunsJSON:      string(runsBytes),
	})
}

func (s *Server) handlePipelineUpdateHeader(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.pipelines.UpdateHeader(r.Context(), id, r.FormValue("name"), r.FormValue("description")); err != nil {
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePipelineDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.pipelines.Delete(r.Context(), id); err != nil && !errors.Is(err, pipelines.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	if isHTMX(r) || acceptsJSON(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.redirect(w, r, "/pipelines?flash=Pipeline+deleted.")
}

func (s *Server) handlePipelineGraphGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	g, err := s.pipelines.GetGraph(r.Context(), id)
	if err != nil {
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(serializeGraph(g))
}

// graphPayload is the JSON shape the editor sends back on Save.
type graphPayload struct {
	Nodes []pipelines.NodeInput `json:"nodes"`
	Edges []pipelines.EdgeInput `json:"edges"`
}

func (s *Server) handlePipelineGraphReplace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	defer r.Body.Close()
	var payload graphPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.pipelines.ReplaceGraph(r.Context(), id, payload.Nodes, payload.Edges); err != nil {
		if errors.Is(err, pipelines.ErrInvalidNode) || errors.Is(err, pipelines.ErrInvalidEdge) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"saved": true})
}

func (s *Server) handlePipelineStartRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.pipelines.StartRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, pipelines.ErrEmptyPipeline) {
			http.Error(w, "pipeline has no nodes", http.StatusBadRequest)
			return
		}
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Execute synchronously in a goroutine so we can return immediately.
	// 5-minute hard cap so a misconfigured pipeline doesn't pin a goroutine forever.
	if s.pipelineExec != nil {
		go func(runID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := s.pipelineExec.Run(ctx, runID); err != nil {
				s.logger.Warn("pipeline run failed", "run", runID, "err", err)
			}
		}(run.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"run_id": run.ID, "status": run.Status})
}

func (s *Server) handlePipelineListRuns(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	limit := parseLimit(r.URL.Query().Get("limit"), 20)
	runs, err := s.pipelines.ListRuns(r.Context(), id, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"runs": serializeRuns(runs)})
}

func (s *Server) handlePipelineRunDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "run_id")
	run, err := s.pipelines.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	steps, _ := s.pipelines.ListSteps(r.Context(), id)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"run":   serializeRun(run),
		"steps": serializeSteps(steps),
	})
}

func (s *Server) handlePipelineRunRollback(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "run_id")
	run, err := s.pipelines.Rollback(r.Context(), id)
	if err != nil {
		if errors.Is(err, pipelines.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"run": serializeRun(run)})
}

// --- serializers ---

func serializeGraph(g pipelines.Graph) map[string]any {
	nodes := make([]map[string]any, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		cfg := "{}"
		if len(n.Config) > 0 {
			cfg = string(n.Config)
		}
		nodes = append(nodes, map[string]any{
			"id":     n.ID,
			"type":   string(n.Type),
			"label":  n.Label,
			"pos_x":  n.PosX,
			"pos_y":  n.PosY,
			"config": json.RawMessage(cfg),
		})
	}
	edges := make([]map[string]any, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, map[string]any{
			"id":        e.ID,
			"source_id": e.SourceID,
			"target_id": e.TargetID,
			"condition": e.Condition,
		})
	}
	return map[string]any{
		"pipeline": map[string]any{
			"id":          g.Pipeline.ID,
			"name":        g.Pipeline.Name,
			"description": g.Pipeline.Description,
			"version":     g.Pipeline.Version,
		},
		"nodes": nodes,
		"edges": edges,
	}
}

func serializeRun(r pipelines.Run) map[string]any {
	out := map[string]any{
		"id":               r.ID,
		"pipeline_id":      r.PipelineID,
		"pipeline_version": r.PipelineVersion,
		"status":           string(r.Status),
		"current_node_id":  r.CurrentNodeID,
		"started_at":       r.StartedAt,
		"error_msg":        r.ErrorMsg,
	}
	if r.EndedAt != nil {
		out["ended_at"] = *r.EndedAt
	}
	return out
}

func serializeRuns(runs []pipelines.Run) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		out = append(out, serializeRun(r))
	}
	return out
}

func serializeSteps(steps []pipelines.Step) []map[string]any {
	out := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		row := map[string]any{
			"id":         s.ID,
			"node_id":    s.NodeID,
			"seq":        s.Seq,
			"status":     string(s.Status),
			"started_at": s.StartedAt,
			"output":     json.RawMessage(strings.TrimSpace(string(s.Output))),
			"error_msg":  s.ErrorMsg,
		}
		if s.EndedAt != nil {
			row["ended_at"] = *s.EndedAt
		}
		out = append(out, row)
	}
	return out
}


