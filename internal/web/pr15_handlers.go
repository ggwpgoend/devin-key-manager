package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ggwpgoend/devin-key-manager/internal/aisearch"
)

// PR-15: AI / search endpoints. All implementations are local, deterministic,
// and dependency-free — see internal/aisearch for the rationale.
//
//   - POST /api/ai/autotag      → suggest tags from a draft prompt
//   - POST /api/ai/tokens       → estimate token count for a draft prompt
//   - GET  /api/ai/suggest      → seed + historical prompt suggestions
//   - GET  /api/ai/similar-tasks → fuzzy similar-task search via local embeddings
//   - PUT  /api/tasks/{id}/tags → persist task tags

type autotagRequest struct {
	Text string `json:"text"`
}

type autotagResponse struct {
	Tags []string `json:"tags"`
}

func (s *Server) handleAIAutotag(w http.ResponseWriter, r *http.Request) {
	var req autotagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(autotagResponse{Tags: aisearch.AutoTag(req.Text)})
}

type tokensRequest struct {
	Text string `json:"text"`
}

type tokensResponse struct {
	Tokens       int  `json:"tokens"`
	Chars        int  `json:"chars"`
	OverWarning  bool `json:"over_warning"`
	OverHardCap  bool `json:"over_hard_cap"`
	WarningLimit int  `json:"warning_limit"`
	HardCap      int  `json:"hard_cap"`
}

func (s *Server) handleAITokens(w http.ResponseWriter, r *http.Request) {
	var req tokensRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// 8 000 tokens is a reasonable "you might want to think about this"
	// threshold; 32 000 is a hard cap based on Claude-style context.
	// These are display hints only — the manager doesn't enforce them.
	const warningLimit = 8000
	const hardCap = 32000
	tokens := aisearch.EstimateTokens(req.Text)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokensResponse{
		Tokens:       tokens,
		Chars:        len(req.Text),
		OverWarning:  tokens >= warningLimit,
		OverHardCap:  tokens >= hardCap,
		WarningLimit: warningLimit,
		HardCap:      hardCap,
	})
}

func (s *Server) handleAISuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := parseLimit(r.URL.Query().Get("limit"), 12)
	all, err := s.tasks.List(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	prompts := make([]string, 0, len(all))
	for _, t := range all {
		prompts = append(prompts, t.InitialPrompt)
	}
	history := aisearch.SuggestFromHistory(prompts, limit/2)
	out := make([]aisearch.Suggestion, 0, limit)
	out = append(out, history...)
	for _, seed := range aisearch.SeedSuggestions {
		if len(out) >= limit {
			break
		}
		out = append(out, seed)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"suggestions": out})
}

// handleAISimilarTasks runs an in-memory cosine search over all tasks.
//
// Why "in-memory and recomputed every request": the user has hundreds of
// tasks at most, vector size is 256 floats, embedding one task takes
// microseconds. Caching adds complexity (invalidation on new tasks) and
// saves us nothing measurable in this user's scale.
func (s *Server) handleAISimilarTasks(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 8)

	ctx := r.Context()
	all, err := s.tasks.List(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	query := aisearch.Embed(aisearch.CleanForEmbed(q))
	type taskResult struct {
		ID     string  `json:"id"`
		Title  string  `json:"title"`
		Prompt string  `json:"prompt"`
		Status string  `json:"status"`
		Tags   string  `json:"tags"`
		Score  float64 `json:"score"`
	}
	scored := make([]aisearch.SearchScore[taskResult], 0, len(all))
	for _, t := range all {
		v := aisearch.Embed(aisearch.CleanForEmbed(t.Title + " " + t.InitialPrompt))
		score := aisearch.Cosine(query, v)
		if score <= 0 {
			continue
		}
		scored = append(scored, aisearch.SearchScore[taskResult]{
			Item: taskResult{
				ID:     t.ID,
				Title:  t.Title,
				Prompt: truncate(t.InitialPrompt, 200),
				Status: string(t.Status),
				Tags:   t.Tags,
			},
			Score: score,
		})
	}
	top := aisearch.TopK(query, scored, limit)
	// Drop very low scores — they're confusing noise.
	out := make([]taskResult, 0, len(top))
	for _, t := range top {
		if t.Score < 0.04 {
			continue
		}
		item := t.Item
		item.Score = t.Score
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tasks": out, "query": q})
}

type setTagsRequest struct {
	Tags string `json:"tags"`
}

func (s *Server) handleSetTaskTags(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	var req setTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.tasks.SetTags(r.Context(), id, req.Tags); err != nil {
		s.serverError(w, r, err)
		return
	}
	t, err := s.tasks.Get(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": t.ID, "tags": t.Tags})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
