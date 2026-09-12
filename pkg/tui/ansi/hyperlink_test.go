package ansi

import (
	"strings"
	"testing"
)

func TestHyperlinksSurviveLayoutAndCloseAtRowBoundaries(t *testing.T) {
	const destination = "file:///tmp/main%20file.go#L42"
	source := Bold + Hyperlink("first 日本語\nsecond line", destination) + Reset
	lines := Wrap(source, 7)
	if len(lines) < 3 {
		t.Fatalf("expected wrapped rows: %q", lines)
	}
	for _, line := range lines {
		if Width(line) > 7 {
			t.Fatalf("hyperlink affected width: %q", line)
		}
		p := &parser{}
		p.feed(line + "X")
		for _, seg := range p.segments[:len(p.segments)-1] {
			if seg.link != destination {
				t.Fatalf("lost hyperlink on %q in %q", seg.text, line)
			}
		}
		if p.link != "" {
			t.Fatalf("hyperlink leaked beyond row: %q", line)
		}
	}
	short := Truncate(source, 6, "…")
	p := &parser{}
	p.feed(short)
	if Width(short) > 6 || p.segments[len(p.segments)-1].link != "" {
		t.Fatalf("truncation linked the ellipsis or exceeded width: %q", short)
	}
	selected := Highlight(lines[0], 0, 3, Reverse)
	if Strip(selected) != Strip(lines[0]) || !strings.Contains(selected, destination) {
		t.Fatalf("selection changed text or lost link: %q", selected)
	}
	if got := CutPlain(lines[0], 0, Width(lines[0])); got != Strip(lines[0]) {
		t.Fatalf("copy exposed hyperlink metadata: %q", got)
	}
}

func TestHyperlinkRejectsTerminalCommandsAndUnknownSchemes(t *testing.T) {
	for _, destination := range []string{"javascript:alert(1)", "https://example.com/\x1b]52;c;payload\a", "file:relative", "https://", ""} {
		if got := Hyperlink("label", destination); got != "label" {
			t.Errorf("unsafe destination %q produced %q", destination, got)
		}
	}
	for _, end := range []string{"\a", "\x1b\\"} {
		p := &parser{}
		p.feed("\x1b]8;id=1;https://example.com" + end + "link\x1b]8;;" + end + "plain")
		if p.segments[0].link != "https://example.com" || p.link != "" {
			t.Fatalf("OSC 8 with terminator %q was not understood", end)
		}
	}
}
