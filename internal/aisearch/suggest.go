package aisearch

import (
	"sort"
	"strings"
)

// Suggestion is one prompt template proposed to the user when they're
// composing a new task. The Source field is one of:
//
//   - "history" — extracted from a prior successful task
//   - "seed"    — built-in starter library
//   - "similar" — pulled from the user's own history by similarity to
//     whatever they've typed so far (handled in the web layer)
type Suggestion struct {
	Text   string `json:"text"`
	Source string `json:"source"`
	Score  float64 `json:"score"`
}

// SeedSuggestions is the small built-in library shown when the user has
// no prior task history. Bilingual, oriented around common Devin asks.
var SeedSuggestions = []Suggestion{
	{Text: "Refactor {file} to extract {function} into its own package and add unit tests.", Source: "seed", Score: 1},
	{Text: "Reproduce the bug in {file}, write a failing test, then fix it.", Source: "seed", Score: 1},
	{Text: "Add a new {feature} endpoint at /api/{path} that returns JSON {shape}.", Source: "seed", Score: 1},
	{Text: "Run `go test ./...` and fix every failure. Don't change test files.", Source: "seed", Score: 1},
	{Text: "Сделай UI для {фича} — на текущем стеке, в storm-палитре, без новых зависимостей.", Source: "seed", Score: 1},
	{Text: "Найди узкое место в {flow} и оптимизируй. Покажи бенчмарки до/после.", Source: "seed", Score: 1},
	{Text: "Покрой пакет {package} тестами. Минимум 80% строк, без интеграционных моков.", Source: "seed", Score: 1},
	{Text: "Document the public API of {package} with godoc comments.", Source: "seed", Score: 1},
}

// SuggestFromHistory extracts useful templated prompts from previous tasks.
//
// Approach: take each historical prompt, normalise it down to a deduplicated
// list of phrases, drop short / generic ones, and return the top scored.
// Score is roughly: log(length) * (1 - duplication-penalty). We avoid
// returning two suggestions whose first 25 chars match.
//
// This is intentionally lightweight (no LLM, no clustering library). It
// surfaces *exact* past prompts as templates so the user can immediately
// see what they wrote before and just edit the deltas.
func SuggestFromHistory(prompts []string, limit int) []Suggestion {
	if limit <= 0 {
		limit = 10
	}
	seen := make(map[string]bool, len(prompts))
	out := make([]Suggestion, 0, limit*2)
	for _, p := range prompts {
		p = strings.TrimSpace(p)
		if len(p) < 25 {
			continue
		}
		// Truncate to 240 chars so the suggestion list stays scannable;
		// the user can expand-into-full later.
		short := p
		if len(short) > 240 {
			short = short[:240] + "…"
		}
		key := strings.ToLower(strings.TrimSpace(short[:25]))
		if seen[key] {
			continue
		}
		seen[key] = true
		score := float64(len(p)) / 240.0
		if score > 1 {
			score = 1
		}
		out = append(out, Suggestion{Text: short, Source: "history", Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SuggestSimilar finds the top-k historical prompts most similar to
// `partial` (the in-progress draft typed by the user). Powers the
// "auto-complete from history" surface shown while typing in the task
// composer.
//
// `corpus` is the precomputed (prompt, vector) tuple slice that the
// caller maintains; we just dot-product against the query embedding.
func SuggestSimilar(partial string, corpus []SearchScore[string], k int) []Suggestion {
	if strings.TrimSpace(partial) == "" || len(corpus) == 0 {
		return nil
	}
	query := Embed(CleanForEmbed(partial))
	// Re-score every corpus entry against the query — the Score field
	// on the input is treated as the precomputed embedding-norm.
	rescored := make([]SearchScore[string], 0, len(corpus))
	for _, it := range corpus {
		// Caller stores the vector via a parallel slice; we don't ship
		// vectors over the score field. This entry point expects the
		// score to already be computed cosine to the query — done in
		// the web layer where the vectors live. We just sort + clip.
		rescored = append(rescored, it)
	}
	top := TopK[string](query, rescored, k)
	out := make([]Suggestion, 0, len(top))
	for _, t := range top {
		if t.Score < 0.05 {
			continue // too dissimilar — would be confusing noise
		}
		out = append(out, Suggestion{Text: t.Item, Source: "similar", Score: t.Score})
	}
	return out
}
