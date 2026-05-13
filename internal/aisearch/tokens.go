// Package aisearch is the "AI / search" toolbelt for the dashboard
// (roadmap items K79, K81, K82, L89).
//
// Everything in this package is **deliberately local and dependency-free**.
// The roadmap explicitly allowed pulling in a free embedding model OR a
// "very small" footprint approach; we chose the latter. Concrete trade-offs:
//
//   - Token estimator (this file): a fast heuristic, NOT an exact tokenizer.
//     Devin doesn't expose token counts per request anyway, so the estimator
//     just helps the user avoid pasting an obviously-too-large prompt.
//
//   - Auto-tag (autotag.go): regex/keyword classifier — predictable,
//     debuggable, no calls out. Misses some edge cases an LLM would catch
//     but never hallucinates.
//
//   - Embedding-search (embed.go): a character-level n-gram hash → vector
//     ("hashing trick"), then cosine similarity. Fits in ~10 KB of state per
//     million tokens of corpus and works fully offline.
//
//   - Prompt suggestions (suggest.go): n-gram template extraction across
//     previous tasks plus a small built-in seed list.
//
// The HTTP layer is wired up in internal/web/pr15_handlers.go.
package aisearch

import (
	"strings"
	"unicode"
)

// EstimateTokens returns a rough approximation of the number of tokens the
// Devin API will see for `text`. The function is intentionally fast (no
// allocations beyond a small running window) and deterministic.
//
// Heuristic:
//
//   - English / Latin: ~4 characters per token (OpenAI's commonly-quoted
//     ratio; Devin reuses Claude-like tokenizers under the hood).
//   - Cyrillic / CJK: ~2 characters per token (these scripts encode less
//     information per byte after UTF-8, and each character tends to map to
//     ~1 token in a BPE vocabulary).
//   - Whitespace/punctuation: counts toward the latin bucket.
//
// We return ints so the UI can display "≈ 312 tokens" without faking
// precision the user might trust too much.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var (
		latin    int
		cyrillic int
		cjk      int
	)
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			cjk++
		default:
			latin++
		}
	}
	// Latin: 4 chars/token; Cyrillic/CJK: 2 chars/token. Add small floor
	// so 1-3 char inputs are reported as 1 token, never 0.
	est := latin/4 + (cyrillic+cjk)/2
	if est == 0 && (latin+cyrillic+cjk) > 0 {
		est = 1
	}
	return est
}

// CountWords is a small helper used by the prompt-suggestion code below.
// We split on whitespace and punctuation, then drop the obvious noise
// tokens. Returns the lowercased remaining words.
func CountWords(text string) []string {
	lower := strings.ToLower(text)
	out := make([]string, 0, len(lower)/6)
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := b.String()
		if !isNoiseWord(w) {
			out = append(out, w)
		}
		b.Reset()
	}
	for _, r := range lower {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// stopwords are filtered out before vectorising / suggestion-extraction so
// the n-grams skew toward signal-bearing tokens. Mixed Russian + English
// because the user codes/comments in both.
var stopwords = map[string]struct{}{
	// English
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {}, "if": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "being": {}, "to": {}, "of": {}, "in": {}, "on": {}, "at": {}, "by": {}, "for": {}, "with": {},
	"as": {}, "from": {}, "into": {}, "that": {}, "this": {}, "it": {}, "i": {}, "you": {}, "we": {}, "he": {}, "she": {},
	"they": {}, "what": {}, "which": {}, "who": {}, "do": {}, "does": {}, "did": {}, "have": {}, "has": {}, "had": {},
	"can": {}, "could": {}, "should": {}, "would": {}, "will": {}, "may": {}, "might": {}, "me": {}, "my": {}, "your": {},
	// Russian
	"и": {}, "в": {}, "не": {}, "на": {}, "с": {}, "что": {}, "это": {}, "как": {}, "по": {}, "из": {}, "у": {},
	"то": {}, "же": {}, "так": {}, "от": {}, "за": {}, "ли": {}, "но": {}, "о": {}, "к": {}, "до": {}, "для": {},
}

func isNoiseWord(w string) bool {
	if len(w) <= 1 {
		return true
	}
	_, ok := stopwords[w]
	return ok
}
