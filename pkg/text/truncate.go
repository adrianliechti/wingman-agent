package text

import (
	"fmt"
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
