package markdown

import (
	"strings"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func hexColor(hex string) ansi.Color {
	return ansi.Hex(hex)
}

// sanitize strips control characters from untrusted text so it cannot inject
// escape sequences, and expands tabs (terminals expand them; the width math
// cannot). Bidirectional and zero-width formatting characters are stripped
// too: they can visually reorder or hide text, letting tool input alter what
// a confirmation prompt appears to say.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, needsSanitize) {
		return s
	}

	// Drop whole escape sequences first; removing just the ESC byte would
	// leave their printable payload ("[?25l") behind as garbage text.
	if strings.ContainsRune(s, 0x1b) {
		s = ansi.Strip(s)
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

// isControl reports control characters (Cc) and invisible formatting
// characters (Cf: bidi overrides, zero-width characters, tag characters).
func isControl(r rune) bool {
	if r == '\n' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
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
