package text

import (
	"fmt"
	"unicode/utf8"
)

// TruncateHead keeps the first maxBytes of s at a UTF-8 boundary and appends
// a "…N chars truncated…" marker. Useful when only the start of the value is
// informative.
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
	// Walk back from maxBytes to the nearest rune start so the cut lands
	// on a UTF-8 boundary.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker(utf8.RuneCountInString(s[cut:]))
}

func marker(removed int) string {
	return fmt.Sprintf("…%d chars truncated…", removed)
}
