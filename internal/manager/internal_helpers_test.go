package manager

import (
	"testing"

	"github.com/ggwpgoend/devin-key-manager/internal/sessions"
)

func TestRoleFromDevinType_KnownAliases(t *testing.T) {
	cases := []struct {
		in   string
		want sessions.Role
	}{
		{"user_message", sessions.RoleUser},
		{"initial_user_message", sessions.RoleUser},
		{"INITIAL_USER_MESSAGE", sessions.RoleUser},
		{"user", sessions.RoleUser},
		{"prompt", sessions.RoleUser},
		{"devin_message", sessions.RoleAssistant},
		{"agent_message", sessions.RoleAssistant},
		{"assistant_message", sessions.RoleAssistant},
		{"assistant", sessions.RoleAssistant},
		{"", sessions.RoleSystem},
		{"system", sessions.RoleSystem},
		{"tool_result", sessions.RoleSystem},
	}
	for _, c := range cases {
		got := roleFromDevinType(c.in)
		if got != c.want {
			t.Errorf("roleFromDevinType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripAttachmentMarkers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no marker",
			in:   "Hello, Devin!",
			want: "Hello, Devin!",
		},
		{
			name: "single marker line",
			in:   "Here is the file you asked for.\nATTACHMENT:{\"url\":\"https://x/y\",\"name\":\"y\"}",
			want: "Here is the file you asked for.",
		},
		{
			name: "multiple markers interleaved",
			in:   "intro\nATTACHMENT:{\"url\":\"a\"}\nmiddle text\nATTACHMENT:{\"url\":\"b\"}\nouter",
			want: "intro\nmiddle text\nouter",
		},
		{
			name: "marker with leading whitespace",
			in:   "see attached:\n   ATTACHMENT:{\"url\":\"a\"}\nthanks",
			want: "see attached:\nthanks",
		},
		{
			name: "empty input untouched",
			in:   "",
			want: "",
		},
		{
			name: "collapses consecutive blank lines",
			in:   "before\n\nATTACHMENT:{\"url\":\"a\"}\n\nafter",
			want: "before\n\nafter",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripAttachmentMarkers(c.in)
			if got != c.want {
				t.Errorf("stripAttachmentMarkers(%q)\n  got:  %q\n  want: %q", c.in, got, c.want)
			}
		})
	}
}
