package artifacts

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// urlPattern matches http(s) URLs embedded in free-form message text. We are
// deliberately permissive on the trailing-character side and trim common
// punctuation afterwards. The pattern handles both bare URLs and markdown
// link/image syntax — the surrounding `]()` characters are stripped during
// post-processing.
var urlPattern = regexp.MustCompile(`https?://[^\s\)\]\}"'<>]+`)

// trailingPunct is the set of characters we trim from the end of an extracted
// URL. Devin's prose habitually ends sentences with a period right after a
// URL, which would otherwise yield invalid links.
const trailingPunct = ".,;:!?\")]}>"

// ExtractedURL describes one URL parsed out of a message body.
type ExtractedURL struct {
	URL      string // canonical absolute URL
	Filename string // best-effort filename derived from the path
}

// ExtractURLs scans the given text for HTTP(S) URLs, normalises the trailing
// punctuation, and returns each URL exactly once (preserving the order of
// first occurrence). The returned slice is empty when no URLs are present.
func ExtractURLs(text string) []ExtractedURL {
	if text == "" {
		return nil
	}
	matches := urlPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]ExtractedURL, 0, len(matches))
	for _, raw := range matches {
		raw = strings.TrimRight(raw, trailingPunct)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			continue
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, ExtractedURL{
			URL:      raw,
			Filename: filenameFromURL(parsed),
		})
	}
	return out
}

// filenameFromURL returns the basename of the URL path, falling back to
// "attachment" when the path component is empty. Query strings and fragments
// are discarded.
func filenameFromURL(u *url.URL) string {
	if u == nil {
		return "attachment"
	}
	base := path.Base(u.Path)
	if base == "/" || base == "." || base == "" {
		return "attachment"
	}
	if unescaped, err := url.PathUnescape(base); err == nil {
		base = unescaped
	}
	return base
}
