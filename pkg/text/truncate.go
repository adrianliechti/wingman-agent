package text

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func TruncateHead(s string, maxBytes int) string {
	if s == "" {
		return ""
	}
	if maxBytes <= 0 {
		return marker(utf8.RuneCountInString(s))
	}
	if len(s) <= maxBytes {
		return s
	}

	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker(utf8.RuneCountInString(s[cut:]))
}

func marker(removed int) string {
	return fmt.Sprintf("…%d chars truncated…", removed)
}

func HeadBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func TailBytes(s string, n int) string {
	i := len(s) - n
	if i <= 0 {
		return s
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// LineCount counts logical lines, excluding the empty suffix after a final newline.
func LineCount(value string) int {
	if value == "" {
		return 0
	}
	lines := strings.Count(value, "\n")
	if !strings.HasSuffix(value, "\n") {
		lines++
	}
	return lines
}

// HeadLines returns the longest prefix of s that fits in n bytes and ends at a
// line boundary, without the trailing newline. A single line longer than n is
// cut at a byte boundary instead, so the result is never empty for non-empty
// input and a positive n.
func HeadLines(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	if cut := strings.LastIndexByte(s[:n+1], '\n'); cut > 0 && strings.Trim(s[:cut], "\n") != "" {
		return s[:cut]
	}
	return HeadBytes(s, n)
}

// TailLines returns the longest suffix of s that fits in n bytes and starts at
// a line boundary. A single line longer than n is cut at a byte boundary
// instead, so the result is never empty for non-empty input and a positive n.
func TailLines(s string, n int) string {
	i := len(s) - n
	if i <= 0 {
		return s
	}
	if n <= 0 {
		return ""
	}
	// A newline at i-1 means s[i:] already starts a line.
	if s[i-1] == '\n' && strings.Trim(s[i:], "\n") != "" {
		return s[i:]
	}
	if cut := strings.IndexByte(s[i:], '\n'); cut >= 0 && strings.Trim(s[i+cut+1:], "\n") != "" {
		return s[i+cut+1:]
	}
	return TailBytes(s, n)
}
