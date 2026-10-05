package code

import (
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const closeButtonWidth = 2

// Reserve the final cell for the button, even when the title is truncated.
func closeHeader(title string, width int) string {
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return colored(theme.Default.Foreground, "×")
	}
	return ansi.Pad(ansi.Truncate(title, width-closeButtonWidth, "…"), width-closeButtonWidth) + " " + colored(theme.Default.Foreground, "×")
}

func scrollMarker(row, rows, offset, total int) string {
	if total <= rows || rows <= 0 {
		return " "
	}
	thumb := max(1, rows*rows/total)
	offset = min(max(offset, 0), total-rows)
	start := offset * (rows - thumb) / (total - rows)
	if row >= start && row < start+thumb {
		return colored(theme.Default.Foreground, "┃")
	}
	return colored(theme.Default.Border, "│")
}

// Content never consumes the scrollbar's cell or carries its style into it.
func scrollbarLine(text string, width int, marker string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Pad(ansi.Truncate(text, width-1, "…"), width-1) + ansi.Reset + marker
}
