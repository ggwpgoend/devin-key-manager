package aisearch

import (
	"regexp"
	"sort"
	"strings"
)

// AutoTag analyses a task's initial prompt and returns the tags it thinks
// fit best. Output is a deterministic, deduplicated, sorted slice — the
// caller can either auto-apply the tags or surface them as suggestions
// for the user to accept.
//
// The classifier is intentionally rule-based:
//
//   - Predictable: the user can read this file and understand why tag X
//     was suggested. No black-box embeddings here.
//   - No outbound calls: works fully offline on the laptop. Matches the
//     "local-first, free" constraint from the roadmap.
//   - Conservative: when in doubt the tag is dropped. False positives
//     pollute the tasks page; false negatives just mean the user manually
//     adds a tag they always wanted to add.
//
// Adding a new category is a one-line addition to autoTagRules.
func AutoTag(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lower := strings.ToLower(text)
	hits := make(map[string]struct{}, 4)
	for _, rule := range autoTagRules {
		if rule.matches(lower) {
			hits[rule.Tag] = struct{}{}
		}
	}
	out := make([]string, 0, len(hits))
	for t := range hits {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

type autoTagRule struct {
	Tag      string
	Keywords []string
	Regex    *regexp.Regexp
}

func (r autoTagRule) matches(lower string) bool {
	for _, kw := range r.Keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	if r.Regex != nil && r.Regex.MatchString(lower) {
		return true
	}
	return false
}

// autoTagRules is the full classifier — order doesn't matter (we dedupe
// at the end). Keywords are checked as substrings (cheap), regexes are
// the fallback for "boundary"-style detection (e.g. function names).
var autoTagRules = []autoTagRule{
	{
		Tag: "bug",
		Keywords: []string{
			"bug", "fix", "broken", "doesn't work", "does not work", "error", "exception", "crash",
			"баг", "ошибка", "поломка", "не работает", "падает", "вылет",
		},
		Regex: regexp.MustCompile(`\b(fix|repair|resolve)\b`),
	},
	{
		Tag: "feature",
		Keywords: []string{
			"feature", "add support", "implement", "build me", "create a", "endpoint", "new page",
			"добавь", "сделай", "реализуй", "новая фича", "новый",
		},
		Regex: regexp.MustCompile(`\badd (a |an |the )?new\b`),
	},
	{
		Tag: "refactor",
		Keywords: []string{
			"refactor", "clean up", "rename", "extract", "split into", "deduplicate",
			"рефактор", "переименуй", "вынеси", "разбей", "почисти",
		},
	},
	{
		Tag: "test",
		Keywords: []string{
			"write tests", "add tests", "unit test", "integration test", "e2e test", "regression test",
			"тест", "тесты", "покрой тестами",
		},
		Regex: regexp.MustCompile(`\btest(s|ing)?\b`),
	},
	{
		Tag: "docs",
		Keywords: []string{
			"document", "readme", "docstring", "comment", "explain in code",
			"документ", "readme", "доку", "комментарии",
		},
	},
	{
		Tag: "ui",
		Keywords: []string{
			"button", "modal", "tooltip", "dropdown", "form", "page layout", "tailwind", "css",
			"кнопка", "форма", "верстка", "интерфейс", "стили",
		},
	},
	{
		Tag: "data",
		Keywords: []string{
			"migration", "schema", "database", "sql", "query", "index", "json", "csv",
			"миграц", "схема", "база", "запрос",
		},
	},
	{
		Tag: "perf",
		Keywords: []string{
			"slow", "performance", "optimize", "bottleneck", "n+1", "profile", "memory leak",
			"тормоз", "медлен", "оптимиз", "профил", "утечк",
		},
	},
	{
		Tag: "security",
		Keywords: []string{
			"auth", "permission", "vulnerab", "csrf", "xss", "sql inject", "encrypt", "secret",
			"авториз", "доступ", "пароль", "шифр", "секрет",
		},
	},
	{
		Tag: "exploration",
		Keywords: []string{
			"explore", "investigate", "research", "find out", "compare", "what is the best way",
			"изучи", "исследуй", "разберись", "сравни", "как лучше",
		},
	},
	{
		Tag: "deploy",
		Keywords: []string{
			"deploy", "docker", "github actions", "ci ", "release", "build pipeline",
			"деплой", "релиз",
		},
	},
}
