package markdown

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func hexColor(hex string) ansi.Color {
	return ansi.Hex(hex)
}

// sanitize strips control characters from untrusted text so it cannot inject
// escape sequences, and expands tabs (terminals expand them; the width math
// cannot).
func sanitize(s string) string {
	if !strings.ContainsFunc(s, needsSanitize) {
		return s
	}

	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			sb.WriteString("  ")
		case isControl(r):
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func needsSanitize(r rune) bool {
	return r == '\t' || isControl(r)
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\n') || r == 0x7f
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
