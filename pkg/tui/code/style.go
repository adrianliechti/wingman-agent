package code

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const cellIndent = "  "

func fg(c ansi.Color) string {
	return ansi.Fg(c)
}

func dim(text string) string {
	return fg(theme.Default.BrBlack) + text + ansi.Reset
}

func bold(text string) string {
	return ansi.Bold + text + ansi.Reset
}

func colored(c ansi.Color, text string) string {
	return fg(c) + text + ansi.Reset
}

// indentWrap wraps styled text and prefixes every line with the standard cell
// indent.
func indentWrap(text string, width int) []string {
	inner := width - len(cellIndent)
	if inner < 10 {
		inner = 10
	}

	var lines []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, wl := range ansi.Wrap(line, inner) {
			lines = append(lines, cellIndent+wl)
		}
	}
	return lines
}

// continuationWrap renders tool output lines under a `└` gutter.
func continuationWrap(text string, width int, colorize func(string) string) []string {
	inner := width - len(cellIndent) - 4
	if inner < 10 {
		inner = 10
	}

	var lines []string
	first := true

	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		wrapped := ansi.Wrap(colorize(line), inner)
		for _, wl := range wrapped {
			if first {
				lines = append(lines, cellIndent+dim("└ ")+wl)
				first = false
			} else {
				lines = append(lines, cellIndent+"  "+wl)
			}
		}
	}
	return lines
}
