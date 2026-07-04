package markdown

import (
	"strings"

	"github.com/rivo/tview"
)

func visibleLen(s string) int {
	return tview.TaggedStringWidth(s)
}

func WrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}

	result := tview.WordWrap(line, width)

	for i, l := range result {
		result[i] = strings.TrimRight(l, " ")
	}

	if len(result) == 0 {
		return []string{line}
	}

	return result
}
