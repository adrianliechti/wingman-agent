package markdown

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

// Tables keep inline styles and links. When columns would turn into narrow
// strips of text, render each record as labeled fields instead.
func (r *ANSIRenderer) renderTable(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		w.WriteString("\n")
		return ast.WalkContinue, nil
	}
	if node.PreviousSibling() != nil {
		w.WriteString("\n")
	}
	table := node.(*east.Table)
	columns := len(table.Alignments)
	widths := make([]int, columns)
	var rows [][]string
	cellRenderer := renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(NewANSIRenderer(r.options), 100)))
	for row := node.FirstChild(); row != nil; row = row.NextSibling() {
		cells := make([]string, columns)
		i := 0
		for cell := row.FirstChild(); cell != nil && i < columns; cell = cell.NextSibling() {
			var out bytes.Buffer
			if err := cellRenderer.Render(&out, source, cell); err != nil {
				return ast.WalkStop, err
			}
			cells[i] = strings.TrimSpace(out.String())
			for line := range strings.SplitSeq(cells[i], "\n") {
				widths[i] = max(widths[i], ansi.Width(line))
			}
			i++
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || columns == 0 {
		return ast.WalkSkipChildren, nil
	}

	budget := r.options.Width - (columns*3 - 1)
	if r.options.Width > 0 {
		minimum := make([]int, columns)
		for i, width := range widths {
			minimum[i] = min(width, 10)
		}
		if sumWidths(minimum) > budget {
			r.renderTableRecords(w, rows)
			return ast.WalkSkipChildren, nil
		}
		// Bound work by the terminal width even for very long unbroken cells.
		for i := range widths {
			widths[i] = min(widths[i], budget)
		}
		for sumWidths(widths) > budget {
			widest := -1
			for i, width := range widths {
				if width > minimum[i] && (widest < 0 || width > widths[widest]) {
					widest = i
				}
			}
			if widest < 0 {
				break
			}
			widths[widest]--
		}
		for _, row := range rows {
			for i, cell := range row {
				if len(ansi.Wrap(cell, max(widths[i], 1))) > 4 {
					r.renderTableRecords(w, rows)
					return ast.WalkSkipChildren, nil
				}
			}
		}
	}
	for rowIndex, row := range rows {
		wrapped := make([][]string, columns)
		height := 1
		for i, cell := range row {
			if rowIndex == 0 {
				cell = ansi.Bold + cell + ansi.Reset
			}
			wrapped[i] = ansi.Wrap(cell, max(widths[i], 1))
			height = max(height, len(wrapped[i]))
		}
		for line := range height {
			for i, lines := range wrapped {
				if i > 0 {
					fmt.Fprintf(w, "%s│%s", r.border(), ansi.Reset)
				}
				text := ""
				if line < len(lines) {
					text = lines[line]
				}
				padding := max(widths[i]-ansi.Width(text), 0)
				left := 0
				switch table.Alignments[i] {
				case east.AlignRight:
					left = padding
				case east.AlignCenter:
					left = padding / 2
				}
				fmt.Fprintf(w, " %s%s%s%s ", strings.Repeat(" ", left), text, ansi.Reset, strings.Repeat(" ", padding-left))
			}
			w.WriteString("\n")
		}
		if rowIndex == 0 {
			for i, width := range widths {
				if i > 0 {
					fmt.Fprintf(w, "%s┼%s", r.border(), ansi.Reset)
				}
				fmt.Fprintf(w, "%s%s%s", r.border(), strings.Repeat("─", width+2), ansi.Reset)
			}
			w.WriteString("\n")
		}
	}
	return ast.WalkSkipChildren, nil
}

func sumWidths(widths []int) int {
	total := 0
	for _, width := range widths {
		total += width
	}
	return total
}

func (r *ANSIRenderer) renderTableRecords(w util.BufWriter, rows [][]string) {
	width := max(r.options.Width, 1)
	for rowIndex, row := range rows[1:] {
		if rowIndex > 0 {
			w.WriteString("\n")
		}
		for i, value := range row {
			label := ansi.Bold + rows[0][i] + ansi.Reset + ": "
			indent := min(2, width-1)
			lines := ansi.Wrap(label+value, max(width-indent, 1))
			for lineIndex, line := range lines {
				if lineIndex > 0 {
					w.WriteString(strings.Repeat(" ", indent))
				}
				w.WriteString(line + ansi.Reset + "\n")
			}
		}
	}
	if len(rows) == 1 {
		for _, label := range rows[0] {
			for _, line := range ansi.Wrap(label, width) {
				w.WriteString(line + ansi.Reset + "\n")
			}
		}
	}
}
