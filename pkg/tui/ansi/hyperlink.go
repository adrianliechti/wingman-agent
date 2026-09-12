package ansi

import (
	"net/url"
	"strings"
	"unicode"
)

const HyperlinkEnd = "\x1b]8;;\x1b\\"

// Hyperlink gives terminal emulators a destination without adding visible text.
// Unsupported schemes and control bytes never become terminal commands.
func Hyperlink(text, destination string) string {
	if !validHyperlink(destination) {
		return text
	}
	return hyperlinkStart(destination) + text + HyperlinkEnd
}

func validHyperlink(destination string) bool {
	if len(destination) > 8192 || strings.ContainsFunc(destination, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(destination)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Host != ""
	case "file":
		return strings.HasPrefix(u.Path, "/")
	}
	return false
}

func hyperlinkStart(destination string) string {
	return "\x1b]8;;" + destination + "\x1b\\"
}

func parseHyperlink(sequence string) (string, bool) {
	if !strings.HasPrefix(sequence, "\x1b]8;") {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimSuffix(sequence[4:], "\x1b\\"), "\a")
	_, destination, ok := strings.Cut(body, ";")
	return destination, ok && (destination == "" || validHyperlink(destination))
}

func changeHyperlink(out *strings.Builder, current *string, next string) {
	if *current == next {
		return
	}
	if *current != "" {
		out.WriteString(HyperlinkEnd)
	}
	if next != "" {
		out.WriteString(hyperlinkStart(next))
	}
	*current = next
}
