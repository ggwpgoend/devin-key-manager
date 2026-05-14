package artifacts

import (
	"strings"
	"testing"
)

func TestUnifiedDiff_NoChange(t *testing.T) {
	got := unifiedDiff([]string{"a", "b", "c"}, []string{"a", "b", "c"}, 3)
	if got != "" {
		t.Fatalf("expected empty diff for identical input, got %q", got)
	}
}

func TestUnifiedDiff_SingleChange(t *testing.T) {
	got := unifiedDiff(
		[]string{"alpha", "bravo", "charlie", "delta"},
		[]string{"alpha", "BRAVO", "charlie", "delta"},
		1,
	)
	if !strings.Contains(got, "-bravo") || !strings.Contains(got, "+BRAVO") {
		t.Fatalf("expected bravo/BRAVO change, got:\n%s", got)
	}
	if !strings.Contains(got, "@@") {
		t.Fatalf("expected hunk header, got:\n%s", got)
	}
}

func TestUnifiedDiff_AddedAndRemoved(t *testing.T) {
	got := unifiedDiff(
		[]string{"keep", "remove-me"},
		[]string{"keep", "add-me", "another"},
		3,
	)
	if !strings.Contains(got, "-remove-me") {
		t.Fatalf("expected deletion of remove-me; got:\n%s", got)
	}
	if !strings.Contains(got, "+add-me") || !strings.Contains(got, "+another") {
		t.Fatalf("expected additions; got:\n%s", got)
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("hello world")) {
		t.Fatalf("text reported as binary")
	}
	if !isBinary([]byte{0, 1, 2, 3}) {
		t.Fatalf("nul-bytes not detected as binary")
	}
}

func TestLCSOps_Determinism(t *testing.T) {
	a := []string{"x", "y", "z"}
	b := []string{"x", "y", "z"}
	ops := lcsOps(a, b)
	if len(ops) != 3 {
		t.Fatalf("expected 3 equal ops, got %d", len(ops))
	}
	for _, op := range ops {
		if op.kind != opEqual {
			t.Fatalf("expected only equals for identical input")
		}
	}
}
