package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (p *Popup) commandList() bool {
	return p.kind == popupCommands || p.kind == popupPalette
}

func commandLine(text string) string {
	return strings.Join(strings.Fields(markdown.Sanitize(text)), " ")
}

// Prefer a command name over an incidental match in its description. Ranking
// chooses the selection without rearranging the visible catalog.
func commandMatchRank(label, query string) int {
	if strings.HasPrefix(label, "/") {
		label, _, _ = strings.Cut(strings.TrimPrefix(label, "/"), " ")
	}
	label = strings.ToLower(label)
	query = strings.ToLower(strings.TrimPrefix(query, "/"))
	if label == query {
		return 0
	}
	if strings.HasPrefix(label, query) {
		return 1
	}
	return 2
}

// Command rows keep their height and columns through filtering and navigation.
// The selected category lives in the footer, so crossing groups doesn't insert
// headings and move the chat or composer. Other pickers retain their own layout.
func (p *Popup) renderCommands(width int) []string {
	if width <= 0 {
		return nil
	}
	t := theme.Default
	inner := max(width-len(cellIndent), 1)
	var lines []string
	appendLine := func(text string) {
		lines = append(lines, cellIndent+ansi.Truncate(text, inner, "…")+ansi.Reset)
	}
	for _, line := range p.header {
		appendLine(line)
	}
	if p.title != "" {
		heading := "  " + dim(commandLine(p.title))
		if p.kind == popupPalette {
			query := dim("type to search")
			if p.query != "" {
				query = colored(t.Foreground, commandLine(p.query))
			}
			heading += "  " + query
		}
		appendLine(heading)
	}

	visible := p.maxRows
	if visible <= 0 {
		visible = popupMaxRows
	}
	visible = max(1, min(visible, len(p.items)))
	if p.index < p.offset {
		p.offset = p.index
	}
	if p.index >= p.offset+visible {
		p.offset = p.index - visible + 1
	}
	p.offset = min(max(p.offset, 0), max(len(p.filtered)-visible, 0))

	// Measure the complete catalog, not the current matches or scroll window.
	labelWidth, shortcutWidth := 0, 0
	for _, item := range p.items {
		labelWidth = max(labelWidth, ansi.Width(commandLine(item.Label)))
		shortcutWidth = max(shortcutWidth, ansi.Width(commandLine(item.Shortcut)))
	}
	if inner < 70 {
		shortcutWidth = 0
	}
	shortcutSpace := 0
	if shortcutWidth > 0 {
		shortcutSpace = min(shortcutWidth, 14) + 2
	}
	labelWidth = min(labelWidth, min(30, inner/3))
	detailWidth := inner - 4 - labelWidth - 2 - shortcutSpace
	if inner < 60 || detailWidth < 16 {
		detailWidth = 0
		labelWidth = max(inner-4-shortcutSpace, 0)
	}

	for row := range visible {
		i := p.offset + row
		if i >= len(p.filtered) {
			if row == 0 {
				appendLine(dim("    No matching commands"))
			} else {
				appendLine("")
			}
			continue
		}
		item := p.items[p.filtered[i]]
		color := t.Foreground
		if item.Disabled {
			color = t.BrBlack
		}
		line := "  "
		if i == p.index {
			line = fg(t.Cyan) + "› "
		}
		check := "  "
		if item.Checked {
			check = colored(t.Cyan, "✓ ")
		}
		line += fg(t.BrBlack) + check + fg(color) + ansi.Pad(ansi.Truncate(commandLine(item.Label), labelWidth, "…"), labelWidth)
		if detailWidth > 0 {
			line += "  " + fg(t.BrBlack) + ansi.Pad(ansi.Truncate(commandLine(item.Detail), detailWidth, "…"), detailWidth)
		}
		if shortcutSpace > 0 {
			shortcut := ansi.Truncate(commandLine(item.Shortcut), shortcutSpace-2, "…")
			line += "  " + fg(t.BrBlack) + strings.Repeat(" ", max(shortcutSpace-2-ansi.Width(shortcut), 0)) + shortcut
		}
		line = ansi.Pad(ansi.Truncate(line, inner, "…"), inner)
		if i == p.index {
			line = selectionLine(line, inner)
		}
		appendLine(line)
	}

	metadata := ""
	if item, ok := p.Current(); ok {
		metadata = item.Group
		if detailWidth == 0 {
			metadata = item.Detail
		}
		if item.Disabled && item.DisabledReason != "" {
			metadata = item.DisabledReason
		}
	}
	position := "0 matches"
	if len(p.filtered) > 0 {
		position = fmt.Sprintf("%d/%d", p.index+1, len(p.filtered))
	}
	leftWidth := max(inner-ansi.Width(position)-2, 0)
	left := ""
	if leftWidth > 0 {
		left = ansi.Pad(ansi.Truncate("    "+commandLine(metadata), leftWidth, "…"), leftWidth) + "  "
	}
	appendLine(dim(left + position))
	return lines
}
