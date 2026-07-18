package markdown

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestWrapLineRespectsVisibleWidth(t *testing.T) {
	cases := []string{
		"building it, saving it to a directory, and having it put next to the existing site [old-site]",
		"I'll put it next to [Adrian's old site] so it's easy to find later on the server",
		"a plain line with no brackets at all that should still wrap on word boundaries",
		"\x1b[36mstyled\x1b[0m text mixed with a [literal] bracket and an [Adrian's] one",
	}

	width := 40

	for _, raw := range cases {
		for _, l := range WrapLine(raw, width) {
			if got := ansi.Width(l); got > width {
				t.Errorf("WrapLine(%q, %d): line %q renders to %d cols", raw, width, l, got)
			}
		}
	}
}

func TestRenderProducesANSI(t *testing.T) {
	out := Render("# Title\n\nSome **bold** and `code`.\n")

	if strings.Contains(out, "[::b]") || strings.Contains(out, "[-]") {
		t.Fatalf("output still contains tview tags: %q", out)
	}
	if !strings.Contains(out, ansi.Bold) {
		t.Fatalf("expected bold SGR in output: %q", out)
	}
}

func TestSanitizeStripsEscapes(t *testing.T) {
	if got := Sanitize("a\x1b[31mred\x07b"); got != "a[31mredb" {
		t.Fatalf("Sanitize = %q", got)
	}
}

func TestSanitizeStripsInvisibleFormatting(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bidi override", "rm /tmp/x\U0000202Egnihtemos\U0000202C", "rm /tmp/xgnihtemos"},
		{"bidi isolate", "a\U00002066hidden\U00002069b", "ahiddenb"},
		{"zero width space", "safe\U0000200B-command", "safe-command"},
		{"zero width joiner", "a\U0000200Db", "ab"},
		{"byte order mark", "\U0000FEFFls -la", "ls -la"},
		{"c1 control", "a\U0000009Bb", "ab"},
		{"tag characters", "cmd\U000E0041\U000E007F", "cmd"},
		{"newline kept", "line1\nline2", "line1\nline2"},
		{"plain text untouched", "echo 'héllo wörld'", "echo 'héllo wörld'"},
	}

	for _, tt := range tests {
		if got := Sanitize(tt.in); got != tt.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}
