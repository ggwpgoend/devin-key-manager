package aisearch_test

import (
	"strings"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/aisearch"
)

func TestEstimateTokens_Latin(t *testing.T) {
	got := aisearch.EstimateTokens("Hello world, this is a sentence.")
	// 33 chars → ~ 33/4 = 8.
	if got < 6 || got > 10 {
		t.Errorf("latin: got %d, want ~8", got)
	}
}

func TestEstimateTokens_Cyrillic(t *testing.T) {
	got := aisearch.EstimateTokens("Привет мир, это предложение для теста.")
	// 38 chars but Cyrillic counted at 2 chars/token → ~ 38/2 = 19.
	if got < 14 || got > 23 {
		t.Errorf("cyrillic: got %d, want ~19", got)
	}
}

func TestEstimateTokens_Mixed(t *testing.T) {
	got := aisearch.EstimateTokens("Hello, мир!")
	if got <= 0 {
		t.Errorf("got %d", got)
	}
}

func TestEstimateTokens_Empty(t *testing.T) {
	if got := aisearch.EstimateTokens(""); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
}

func TestEstimateTokens_OneChar(t *testing.T) {
	if got := aisearch.EstimateTokens("x"); got != 1 {
		t.Errorf("one char: got %d, want 1", got)
	}
}

func TestAutoTag_Bug(t *testing.T) {
	tags := aisearch.AutoTag("Fix the broken authentication flow, it crashes on login.")
	if !contains(tags, "bug") {
		t.Errorf("expected 'bug' in %v", tags)
	}
}

func TestAutoTag_Feature(t *testing.T) {
	tags := aisearch.AutoTag("Add a new /api/users endpoint that returns user JSON.")
	if !contains(tags, "feature") {
		t.Errorf("expected 'feature' in %v", tags)
	}
}

func TestAutoTag_Russian(t *testing.T) {
	tags := aisearch.AutoTag("Покрой пакет тестами на 80%.")
	if !contains(tags, "test") {
		t.Errorf("expected 'test' in %v, got %v", tags, tags)
	}
}

func TestAutoTag_Multiple(t *testing.T) {
	tags := aisearch.AutoTag("Fix the slow query in the keys table, and add tests for the new index.")
	for _, want := range []string{"bug", "perf", "data", "test"} {
		if !contains(tags, want) {
			t.Errorf("missing %q in %v", want, tags)
		}
	}
}

func TestAutoTag_Empty(t *testing.T) {
	if tags := aisearch.AutoTag(""); len(tags) != 0 {
		t.Errorf("expected no tags, got %v", tags)
	}
}

func TestEmbedAndCosine_Same(t *testing.T) {
	a := aisearch.Embed("Fix the authentication bug in the login flow.")
	if got := aisearch.Cosine(a, a); got < 0.99 {
		t.Errorf("self-similarity should be ~1, got %v", got)
	}
}

func TestEmbedAndCosine_Similar(t *testing.T) {
	a := aisearch.Embed("Fix the authentication bug in the login flow.")
	b := aisearch.Embed("Authentication is broken when users log in.")
	c := aisearch.Embed("Refactor the database migration scripts to be idempotent.")
	if aisearch.Cosine(a, b) < aisearch.Cosine(a, c) {
		t.Errorf("auth-related docs should be closer than refactor doc")
	}
}

func TestEmbedAndCosine_Empty(t *testing.T) {
	a := aisearch.Embed("")
	b := aisearch.Embed("anything")
	if aisearch.Cosine(a, b) != 0 {
		t.Errorf("empty vector should cosine 0")
	}
}

func TestCleanForEmbed_StripsCodeFences(t *testing.T) {
	got := aisearch.CleanForEmbed("explain why ```go\nfunc x() {}\n``` panics")
	if strings.Contains(got, "func x") {
		t.Errorf("code fence not stripped: %q", got)
	}
}

func TestCleanForEmbed_StripsURLs(t *testing.T) {
	got := aisearch.CleanForEmbed("see https://example.com/foo for details")
	if strings.Contains(got, "https") {
		t.Errorf("URL not stripped: %q", got)
	}
}

func TestSuggestFromHistory_DedupesByPrefix(t *testing.T) {
	prompts := []string{
		"Refactor the authentication flow to use middleware",
		"Refactor the authentication flow to use a separate package",
		"Add tests for the new bulk-import feature for keys",
		"short", // dropped (too short)
	}
	got := aisearch.SuggestFromHistory(prompts, 10)
	if len(got) != 2 {
		t.Errorf("expected 2 deduped suggestions, got %d: %+v", len(got), got)
	}
}

func TestTopK_Sort(t *testing.T) {
	q := aisearch.Embed("query")
	items := []aisearch.SearchScore[string]{
		{Item: "a", Score: 0.1},
		{Item: "b", Score: 0.9},
		{Item: "c", Score: 0.5},
		{Item: "d", Score: 0.7},
	}
	top := aisearch.TopK[string](q, items, 2)
	if len(top) != 2 {
		t.Fatalf("got %d", len(top))
	}
	if top[0].Item != "b" || top[1].Item != "d" {
		t.Errorf("wrong order: %+v", top)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
