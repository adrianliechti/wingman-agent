package code

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const cellIndent = ""

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

// Paint complete rows with an explicit foreground as well as a background:
// terminal-default text may be unreadable on a painted panel after an SGR reset.
func surfaceLine(text string, width int, background ansi.Color) string {
	base := fg(theme.Default.Foreground) + ansi.Bg(background)
	line := ansi.Pad(ansi.Truncate(text, width, "…"), width)
	return ansi.Reset + base + strings.ReplaceAll(line, ansi.Reset, ansi.Reset+base) + ansi.Reset
}

func panelLine(text string, width int) string {
	return surfaceLine(text, width, theme.Default.Surface)
}

func selectionLine(text string, width int) string {
	return surfaceLine(text, width, theme.Default.Selection)
}

// indentWrap wraps styled text and prefixes every line with the standard cell
// indent.
func indentWrap(text string, width int) []string {
	inner := max(width-len(cellIndent), 10)

	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		for _, wl := range ansi.Wrap(line, inner) {
			lines = append(lines, cellIndent+wl)
		}
	}
	return lines
}

// continuationWrap renders tool output lines under a `└` gutter.
func continuationWrap(text string, width int, colorize func(string) string) []string {
	inner := max(width-len(cellIndent)-4, 10)

	var lines []string
	first := true

	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
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
