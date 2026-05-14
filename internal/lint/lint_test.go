package lint_test

import (
	"strings"
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/lint"
)

func TestDetectLanguage(t *testing.T) {
	cases := map[string]lint.Language{
		"main.go":     lint.LangGo,
		"foo/bar.py":  lint.LangPython,
		"x.ts":        lint.LangTypeScript,
		"deploy.sh":   lint.LangShell,
		"file.exe":    lint.LangBinary,
		"image.png":   lint.LangBinary,
		"unknown.txt": lint.LangText,
		"Makefile":    lint.LangText,
	}
	for path, want := range cases {
		if got := lint.DetectLanguage(path); got != want {
			t.Errorf("DetectLanguage(%q) = %s, want %s", path, got, want)
		}
	}
}

func TestRun_LongLine(t *testing.T) {
	content := strings.Repeat("a", 150)
	findings := lint.Run("foo.txt", content, lint.Config{MaxLineLength: 120})
	if !hasCode(findings, "long-line") {
		t.Errorf("expected long-line finding, got %+v", findings)
	}
}

func TestRun_MixedLineEndings(t *testing.T) {
	content := "line1\r\nline2\nline3\r\n"
	findings := lint.Run("foo.txt", content, lint.Config{})
	if !hasCode(findings, "mixed-line-endings") {
		t.Errorf("expected mixed-line-endings, got %+v", findings)
	}
}

func TestRun_PythonMixedIndent_Error(t *testing.T) {
	content := "def x():\n    a = 1\n\tb = 2\n"
	findings := lint.Run("a.py", content, lint.Config{})
	if !hasCode(findings, "mixed-indentation") {
		t.Errorf("expected mixed-indentation, got %+v", findings)
	}
}

func TestRun_GoFmtPrintln(t *testing.T) {
	content := "package x\nfunc f() {\nfmt.Println(\"debug\")\n}\n"
	findings := lint.Run("a.go", content, lint.Config{})
	if !hasCode(findings, "go-debug-println") {
		t.Errorf("expected go-debug-println, got %+v", findings)
	}
}

func TestRun_TodoMarker(t *testing.T) {
	content := "// TODO: think about edge case\nfunc f() {}\n"
	findings := lint.Run("a.go", content, lint.Config{})
	if !hasCode(findings, "todo-marker") {
		t.Errorf("expected todo-marker, got %+v", findings)
	}
}

func TestRun_Binary(t *testing.T) {
	findings := lint.Run("photo.png", "anything", lint.Config{})
	if !hasCode(findings, "binary") {
		t.Errorf("expected binary, got %+v", findings)
	}
}

func TestRun_PythonBareExcept(t *testing.T) {
	content := "try:\n    do_thing()\nexcept:\n    pass\n"
	findings := lint.Run("a.py", content, lint.Config{})
	if !hasCode(findings, "py-bare-except") {
		t.Errorf("expected py-bare-except, got %+v", findings)
	}
}

func TestRun_Clean(t *testing.T) {
	content := "package x\n\nfunc f() int { return 1 }\n"
	findings := lint.Run("a.go", content, lint.Config{})
	for _, f := range findings {
		if f.Severity == lint.SeverityError {
			t.Errorf("clean file should not produce errors, got %+v", f)
		}
	}
}

func TestCounts(t *testing.T) {
	findings := []lint.Finding{
		{Severity: lint.SeverityError},
		{Severity: lint.SeverityWarn},
		{Severity: lint.SeverityWarn},
		{Severity: lint.SeverityInfo},
	}
	e, w, i := lint.Counts(findings)
	if e != 1 || w != 2 || i != 1 {
		t.Errorf("Counts: got (%d,%d,%d), want (1,2,1)", e, w, i)
	}
}

func hasCode(findings []lint.Finding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
