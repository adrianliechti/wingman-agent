package markdown

import (
	"testing"

	"github.com/rivo/tview"
)

func TestWrapLineRespectsTaggedWidth(t *testing.T) {
	cases := []string{
		"building it, saving it to a directory, and having it put next to the existing site [old-site]",
		"I'll put it next to [Adrian's old site] so it's easy to find later on the server",
		"a plain line with no brackets at all that should still wrap on word boundaries",
		"[cyan]styled[-] text mixed with a [literal] bracket and an [Adrian's] one",
	}

	width := 40

	for _, raw := range cases {
		escaped := tview.Escape(raw)

		for _, l := range WrapLine(escaped, width) {
			if got := tview.TaggedStringWidth(l); got > width {
				t.Errorf("WrapLine(%q, %d): line %q renders to %d cols", escaped, width, l, got)
			}
			if got := visibleLen(l); got != tview.TaggedStringWidth(l) {
				t.Errorf("visibleLen(%q) = %d, want %d", l, got, tview.TaggedStringWidth(l))
			}
		}
	}
}
