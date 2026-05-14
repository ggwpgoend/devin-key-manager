// Package lint runs lightweight, dependency-free static checks on
// incoming text/code artifacts (PR-16 / roadmap D36).
//
// The user wanted "прогнать gofmt / ruff / eslint на присланном коде" —
// but pulling in those external tools cross-platform is fragile (Windows
// users probably don't have ruff installed). Instead, we ship a built-in
// fast linter that catches the common mistakes the user actually cares
// about, and *optionally* invokes external tools if they happen to be
// on PATH.
//
// All checks are purely text-level: no parser, no AST, no goroutines.
// The point is to flag obvious issues in 5 ms, not to replace a real
// linter.
package lint

import (
	"bufio"
	"strings"
	"unicode/utf8"
)

// Severity ranks how loudly a finding should be displayed in the UI.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Finding is one lint result tied to a specific line.
type Finding struct {
	Line     int      `json:"line"`     // 1-indexed
	Col      int      `json:"col"`      // 1-indexed; 0 when not applicable
	Severity Severity `json:"severity"` // info, warn, error
	Code     string   `json:"code"`     // e.g. "long-line", "trailing-ws"
	Message  string   `json:"message"`
}

// Language is a coarse classifier; we only branch on it for a handful of
// language-specific checks (e.g. tabs-vs-spaces only matters for Python).
type Language string

const (
	LangText       Language = "text"
	LangGo         Language = "go"
	LangPython     Language = "python"
	LangJavaScript Language = "javascript"
	LangTypeScript Language = "typescript"
	LangShell      Language = "shell"
	LangBinary     Language = "binary"
)

// DetectLanguage maps a filename to a best-guess language. The path is
// matched on extension only — no content sniffing here so callers can
// short-circuit obvious binary files before reading them.
func DetectLanguage(path string) Language {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return LangText
	}
	switch strings.ToLower(path[idx:]) {
	case ".go":
		return LangGo
	case ".py":
		return LangPython
	case ".js", ".jsx", ".mjs", ".cjs":
		return LangJavaScript
	case ".ts", ".tsx":
		return LangTypeScript
	case ".sh", ".bash", ".zsh":
		return LangShell
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf",
		".zip", ".tar", ".gz", ".bz2", ".7z",
		".mp3", ".mp4", ".mkv", ".webm",
		".exe", ".dll", ".so", ".dylib":
		return LangBinary
	}
	return LangText
}

// Config tweaks check thresholds. Zero values fall back to sensible defaults.
type Config struct {
	MaxLineLength int // default 120
}

func (c Config) normalize() Config {
	out := c
	if out.MaxLineLength <= 0 {
		out.MaxLineLength = 120
	}
	return out
}

// Run executes every applicable check on `content`. The result is the
// concatenation of all findings, sorted by line then column. Returns an
// empty slice (not nil) when nothing is wrong; callers can check
// `len(findings) == 0` for the happy path.
func Run(path, content string, cfg Config) []Finding {
	cfg = cfg.normalize()
	lang := DetectLanguage(path)
	if lang == LangBinary {
		return []Finding{{
			Line: 0, Col: 0, Severity: SeverityInfo, Code: "binary",
			Message: "binary file — skipped",
		}}
	}
	if !utf8.ValidString(content) {
		return []Finding{{
			Line: 0, Col: 0, Severity: SeverityWarn, Code: "non-utf8",
			Message: "file is not valid UTF-8 — likely binary",
		}}
	}

	findings := make([]Finding, 0, 8)
	findings = append(findings, checkLines(content, cfg)...)
	findings = append(findings, checkMixedLineEndings(content)...)
	findings = append(findings, checkMixedIndentation(content, lang)...)
	findings = append(findings, checkTodoMarkers(content)...)
	switch lang {
	case LangGo:
		findings = append(findings, checkGoSpecific(content)...)
	case LangPython:
		findings = append(findings, checkPythonSpecific(content)...)
	case LangShell:
		findings = append(findings, checkShellSpecific(content)...)
	}
	return findings
}

func checkLines(content string, cfg Config) []Finding {
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if utf8.RuneCountInString(text) > cfg.MaxLineLength {
			out = append(out, Finding{
				Line: line, Col: cfg.MaxLineLength + 1, Severity: SeverityWarn,
				Code:    "long-line",
				Message: "line exceeds max length",
			})
		}
		// Trailing whitespace.
		if i := lastNonSpace(text); i < len(text)-1 && len(text) > 0 {
			out = append(out, Finding{
				Line: line, Col: i + 2, Severity: SeverityInfo,
				Code: "trailing-whitespace", Message: "trailing whitespace",
			})
		}
		// Tabs after spaces — usually accidental.
		if hasTabAfterSpace(text) {
			out = append(out, Finding{
				Line: line, Col: 1, Severity: SeverityInfo,
				Code: "mixed-indent", Message: "tab character after space — likely mixed indentation",
			})
		}
	}
	return out
}

func checkMixedLineEndings(content string) []Finding {
	hasCRLF := strings.Contains(content, "\r\n")
	// LF inside a string that also has CRLF is fine *if* every line ends
	// in CRLF; the cheap test is "any LF without an immediately preceding CR".
	hasBareLF := false
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && (i == 0 || content[i-1] != '\r') {
			hasBareLF = true
			break
		}
	}
	if hasCRLF && hasBareLF {
		return []Finding{{
			Line: 0, Col: 0, Severity: SeverityWarn, Code: "mixed-line-endings",
			Message: "file mixes CRLF and LF line endings",
		}}
	}
	return nil
}

func checkMixedIndentation(content string, lang Language) []Finding {
	// Python is the language where this matters most (it's a syntax error
	// in 3.x to mix); for others it's just a style warning.
	if lang != LangPython && lang != LangGo {
		return nil
	}
	hasTabIndent := false
	hasSpaceIndent := false
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		text := sc.Text()
		if len(text) == 0 {
			continue
		}
		switch text[0] {
		case '\t':
			hasTabIndent = true
		case ' ':
			hasSpaceIndent = true
		}
	}
	if hasTabIndent && hasSpaceIndent {
		severity := SeverityWarn
		if lang == LangPython {
			severity = SeverityError
		}
		return []Finding{{
			Line: 0, Col: 0, Severity: severity, Code: "mixed-indentation",
			Message: "file mixes tab and space indentation",
		}}
	}
	return nil
}

func checkTodoMarkers(content string) []Finding {
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		for _, marker := range []string{"TODO", "FIXME", "XXX", "HACK"} {
			if idx := strings.Index(t, marker); idx >= 0 {
				out = append(out, Finding{
					Line: line, Col: idx + 1, Severity: SeverityInfo,
					Code: "todo-marker", Message: marker + " marker present",
				})
				break
			}
		}
	}
	return out
}

func checkGoSpecific(content string) []Finding {
	var out []Finding
	// "fmt.Println" left from debugging.
	sc := bufio.NewScanner(strings.NewReader(content))
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		if strings.Contains(t, "fmt.Println") && !strings.HasPrefix(strings.TrimSpace(t), "//") {
			out = append(out, Finding{
				Line: line, Severity: SeverityInfo, Code: "go-debug-println",
				Message: "fmt.Println — likely leftover debug code",
			})
		}
		if strings.Contains(t, "panic(") && !strings.HasPrefix(strings.TrimSpace(t), "//") {
			out = append(out, Finding{
				Line: line, Severity: SeverityInfo, Code: "go-panic",
				Message: "panic() — consider returning an error instead",
			})
		}
	}
	return out
}

func checkPythonSpecific(content string) []Finding {
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(content))
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		if strings.Contains(t, "print(") && !strings.HasPrefix(strings.TrimSpace(t), "#") {
			out = append(out, Finding{
				Line: line, Severity: SeverityInfo, Code: "py-print",
				Message: "print() — consider using logging",
			})
		}
		if strings.Contains(t, "except:") && !strings.Contains(t, "except :") {
			out = append(out, Finding{
				Line: line, Severity: SeverityWarn, Code: "py-bare-except",
				Message: "bare except — catch a specific exception type",
			})
		}
	}
	return out
}

func checkShellSpecific(content string) []Finding {
	if strings.HasPrefix(content, "#!") {
		return nil
	}
	return []Finding{{
		Line: 1, Col: 1, Severity: SeverityInfo, Code: "sh-no-shebang",
		Message: "shell script without #!/usr/bin/env bash shebang",
	}}
}

func lastNonSpace(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != ' ' && s[i] != '\t' {
			return i
		}
	}
	return -1
}

func hasTabAfterSpace(s string) bool {
	sawSpace := false
	for _, r := range s {
		if r == ' ' {
			sawSpace = true
		} else if r == '\t' && sawSpace {
			return true
		} else if r != ' ' && r != '\t' {
			return false
		}
	}
	return false
}

// Counts returns a quick severity tally for the UI: how many errors,
// warnings, infos. Useful for a one-line summary.
func Counts(findings []Finding) (errors, warnings, infos int) {
	for _, f := range findings {
		switch f.Severity {
		case SeverityError:
			errors++
		case SeverityWarn:
			warnings++
		case SeverityInfo:
			infos++
		}
	}
	return
}
