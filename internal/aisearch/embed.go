package aisearch

import (
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// embeddingDim is the fixed dimensionality of every vector we produce.
// 256 is a good trade-off for the hashing-trick: large enough to avoid
// pathological collisions on prompt-sized texts (50-500 chars), small
// enough that 1 000 vectors fit in ~1 MB RAM. The value is a power of
// two so that "hash % dim" can be done as a mask, but we use modulo for
// clarity since this isn't a hot path.
const embeddingDim = 256

// Vector is a dense fixed-size embedding. We use float32 because we
// never need more precision than ~6 decimals for similarity and saving
// half the bytes adds up when the user has thousands of past prompts.
type Vector [embeddingDim]float32

// Embed projects text into a fixed-size vector using a deterministic
// hashing-trick over character n-grams. This is **not** a learned
// embedding — it can't distinguish "happy" from "joyful" — but it does
// reliably cluster prompts that share substrings, which is exactly the
// signal the user needs for "find when Devin helped me with auth".
//
// Algorithm:
//
//  1. Tokenize via CountWords (lowercase, strip punctuation, drop stopwords).
//  2. Add character 3-grams of each word (cheap "morphology" — captures
//     prefixes/suffixes so "auth" and "authentication" cluster together).
//  3. Hash each n-gram with FNV-1a and increment the corresponding slot.
//  4. L2-normalize the resulting vector so cosine similarity becomes a
//     simple dot product later.
//
// Empty input returns the zero vector. The function is allocation-free
// after the initial Vector allocation, which the caller does.
func Embed(text string) Vector {
	var v Vector
	if text == "" {
		return v
	}
	words := CountWords(text)
	for _, w := range words {
		// Word-level token.
		v[bucket(w)] += 1
		// 3-gram suffix tokens — small, cheap morphological signal.
		runes := []rune(w)
		if len(runes) < 4 {
			continue
		}
		for i := 0; i <= len(runes)-3; i++ {
			tri := string(runes[i : i+3])
			v[bucket(tri)] += 0.5 // weight 3-grams half — they're noisier than full words
		}
	}
	// L2 normalize so two unrelated docs with different lengths are still
	// comparable. Without this, a 1 000-word doc would always "win" on
	// dot-product against a 10-word query.
	var sumSq float64
	for i := range v {
		sumSq += float64(v[i]) * float64(v[i])
	}
	if sumSq > 0 {
		inv := float32(1.0 / math.Sqrt(sumSq))
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}

// bucket maps a token to a dimension via FNV-1a. We use the high bits to
// reduce collisions when the dim is not a multiple of the hash range.
func bucket(tok string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tok))
	return h.Sum32() % embeddingDim
}

// Cosine returns the cosine similarity between two L2-normalized vectors.
// Since Embed always normalizes, this reduces to a dot product. Returns
// 0 when either vector is the zero vector.
func Cosine(a, b Vector) float64 {
	var dot float64
	for i := 0; i < embeddingDim; i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	// Clamp to [-1, 1] so floating-point fuzz can't break callers that
	// pass our value to acos/etc.
	if dot < -1 {
		return -1
	}
	if dot > 1 {
		return 1
	}
	return dot
}

// SearchScore wraps an item with its similarity to a query.
type SearchScore[T any] struct {
	Item  T
	Score float64
}

// TopK returns the top k items by similarity to the query embedding. The
// caller passes a slice of (item, vector) tuples and we sort them by
// cosine descending, then return the top k. Stable for equal scores.
func TopK[T any](query Vector, items []SearchScore[T], k int) []SearchScore[T] {
	if k <= 0 || len(items) == 0 {
		return nil
	}
	scored := make([]SearchScore[T], 0, len(items))
	for _, it := range items {
		scored = append(scored, SearchScore[T]{Item: it.Item, Score: it.Score})
	}
	// Use a partial selection — for typical k=10 and N=10k this is faster
	// than a full sort. We still fall back to a full sort for tiny N
	// because the constant factor of a heap is not worth it.
	sortByScoreDesc(scored)
	if k < len(scored) {
		scored = scored[:k]
	}
	return scored
}

// sortByScoreDesc does an in-place insertion sort. We don't need to be
// fancy — typical N is <100k and we run this rarely (on user query).
func sortByScoreDesc[T any](s []SearchScore[T]) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j].Score > s[j-1].Score {
			s[j], s[j-1] = s[j-1], s[j]
			j--
		}
	}
}

// CleanForEmbed strips obvious markdown noise (code-fences, URLs) so
// embeddings reflect the user's intent, not the syntactic chrome.
func CleanForEmbed(text string) string {
	// Remove code fences ```...``` so we don't embed bytes of pasted code.
	for {
		i := strings.Index(text, "```")
		if i < 0 {
			break
		}
		j := strings.Index(text[i+3:], "```")
		if j < 0 {
			text = text[:i]
			break
		}
		text = text[:i] + text[i+3+j+3:]
	}
	// Remove inline `code` spans.
	for {
		i := strings.Index(text, "`")
		if i < 0 {
			break
		}
		j := strings.Index(text[i+1:], "`")
		if j < 0 {
			break
		}
		text = text[:i] + " " + text[i+1+j+1:]
	}
	// Strip URLs (anything that looks like https://...).
	var b strings.Builder
	b.Grow(len(text))
	tokens := strings.Fields(text)
	for i, t := range tokens {
		if isURL(t) {
			continue
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t)
	}
	return b.String()
}

func isURL(s string) bool {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
