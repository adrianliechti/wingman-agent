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
