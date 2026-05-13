package keys_test

import (
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/keys"
)

func TestParseBulk_AllFormats(t *testing.T) {
	in := `
# trial pool exported from 1Password
dev-just-the-key

trial-1 dev-with-label-1
trial-2: dev-with-label-2
team-1 : paid : dev-team-1
team-2:free:dev-team-2

trial-3:bogus-plan:dev-trial-3
trial-4:
   dev-spaces-trimmed
trial-with-colons:trial:dev-has:colons:inside
`
	got := keys.ParseBulk(in)

	type want struct {
		LineNo int
		Label  string
		Plan   keys.Plan
		APIKey string
		Err    string
	}
	wants := []want{
		{LineNo: 3, Label: "imported-1", APIKey: "dev-just-the-key"},
		{LineNo: 5, Label: "trial-1", APIKey: "dev-with-label-1"},
		{LineNo: 6, Label: "trial-2", APIKey: "dev-with-label-2"},
		{LineNo: 7, Label: "team-1", Plan: keys.PlanPaid, APIKey: "dev-team-1"},
		{LineNo: 8, Label: "team-2", Plan: keys.PlanFree, APIKey: "dev-team-2"},
		{LineNo: 10, Label: "trial-3", Plan: keys.Plan("bogus-plan"), APIKey: "dev-trial-3", Err: "unknown plan bogus-plan"},
		{LineNo: 11, Label: "trial-4", APIKey: "", Err: "missing api key"},
		{LineNo: 12, Label: "imported-2", APIKey: "dev-spaces-trimmed"},
		{LineNo: 13, Label: "trial-with-colons", Plan: keys.PlanTrial, APIKey: "dev-has:colons:inside"},
	}

	if len(got) != len(wants) {
		t.Fatalf("expected %d lines, got %d: %+v", len(wants), len(got), got)
	}
	for i, w := range wants {
		g := got[i]
		if g.LineNo != w.LineNo {
			t.Errorf("line[%d] no: got %d, want %d", i, g.LineNo, w.LineNo)
		}
		if g.Label != w.Label {
			t.Errorf("line[%d] label: got %q, want %q", i, g.Label, w.Label)
		}
		if g.Plan != w.Plan {
			t.Errorf("line[%d] plan: got %q, want %q", i, g.Plan, w.Plan)
		}
		if g.APIKey != w.APIKey {
			t.Errorf("line[%d] key: got %q, want %q", i, g.APIKey, w.APIKey)
		}
		if g.Error != w.Err {
			t.Errorf("line[%d] err: got %q, want %q", i, g.Error, w.Err)
		}
	}
}

func TestParseBulk_Empty(t *testing.T) {
	for _, in := range []string{"", "\n\n\n", "# only comments\n", "   \t  \n  "} {
		got := keys.ParseBulk(in)
		if len(got) != 0 {
			t.Errorf("expected empty result for %q, got %+v", in, got)
		}
	}
}
