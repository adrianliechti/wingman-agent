package markdown

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func hexColor(hex string) ansi.Color {
	return ansi.Hex(hex)
}

// sanitize strips control characters (except tab) from untrusted text so it
// cannot inject escape sequences into the rendered output.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}

	var sb strings.Builder
	for _, r := range s {
		if isControl(r) {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f
}

func visibleLen(s string) int {
	return ansi.Width(s)
}

func WrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}

	result := ansi.Wrap(line, width)

	for i, l := range result {
		result[i] = strings.TrimRight(l, " ")
	}

	if len(result) == 0 {
		return []string{line}
	}

	return result
}

// Sanitize is the exported form used by cell formatters for tool output.
func Sanitize(s string) string {
	return sanitize(s)
}
