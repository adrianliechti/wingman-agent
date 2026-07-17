package markdown

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type ANSIRenderer struct {
	theme theme.Theme
}

func NewANSIRenderer() *ANSIRenderer {
	return &ANSIRenderer{
		theme: theme.Default,
	}
}

func (r *ANSIRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindDocument, r.renderDocument)
	reg.Register(ast.KindHeading, r.renderHeading)
	reg.Register(ast.KindBlockquote, r.renderBlockquote)
	reg.Register(ast.KindCodeBlock, r.renderCodeBlock)
	reg.Register(ast.KindFencedCodeBlock, r.renderFencedCodeBlock)
	reg.Register(ast.KindParagraph, r.renderParagraph)
	reg.Register(ast.KindList, r.renderList)
	reg.Register(ast.KindListItem, r.renderListItem)
	reg.Register(ast.KindThematicBreak, r.renderThematicBreak)
	reg.Register(ast.KindHTMLBlock, r.renderHTMLBlock)

	reg.Register(ast.KindText, r.renderText)
	reg.Register(ast.KindString, r.renderString)
	reg.Register(ast.KindCodeSpan, r.renderCodeSpan)
	reg.Register(ast.KindEmphasis, r.renderEmphasis)
	reg.Register(ast.KindLink, r.renderLink)
	reg.Register(ast.KindAutoLink, r.renderAutoLink)
	reg.Register(ast.KindImage, r.renderImage)
	reg.Register(ast.KindRawHTML, r.renderRawHTML)

	reg.Register(east.KindTable, r.renderTable)
	reg.Register(east.KindTableHeader, r.renderTableHeader)
	reg.Register(east.KindTableRow, r.renderTableRow)
	reg.Register(east.KindTableCell, r.renderTableCell)
	reg.Register(east.KindStrikethrough, r.renderStrikethrough)
	reg.Register(east.KindTaskCheckBox, r.renderTaskCheckBox)
}

func (r *ANSIRenderer) dim() string {
	return ansi.Fg(r.theme.BrBlack)
}

func (r *ANSIRenderer) renderDocument(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderHeading(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Heading)

	if entering {
		if node.PreviousSibling() != nil {
			w.WriteString("\n")
		}
		fmt.Fprintf(w, "%s%s%s ", r.dim(), strings.Repeat("#", n.Level), ansi.Reset+ansi.Bold)
	} else {
		w.WriteString(ansi.Reset + "\n")
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderBlockquote(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if node.PreviousSibling() != nil {
			w.WriteString("\n")
		}
		fmt.Fprintf(w, "%s> %s", r.dim(), ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderCodeBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if node.PreviousSibling() != nil {
			w.WriteString("\n")
		}
		n := node.(*ast.CodeBlock)
		var code strings.Builder
		lines := n.Lines()

		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			code.Write(line.Value(source))
		}
		w.WriteString(formatCodeBlock(code.String(), "", r.theme))
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderFencedCodeBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if node.PreviousSibling() != nil {
			w.WriteString("\n")
		}
		n := node.(*ast.FencedCodeBlock)
		lang := ""

		if n.Info != nil {
			lang = string(n.Info.Segment.Value(source))
			if idx := strings.IndexByte(lang, ' '); idx >= 0 {
				lang = lang[:idx]
			}
		}
		var code strings.Builder
		lines := n.Lines()

		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			code.Write(line.Value(source))
		}
		w.WriteString(formatCodeBlock(code.String(), lang, r.theme))
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderParagraph(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		w.WriteString("\n")
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderList(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if node.PreviousSibling() != nil {
			if _, isListItem := node.Parent().(*ast.ListItem); !isListItem {
				w.WriteString("\n")
			}
		}
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderListItem(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.ListItem)
		parent := n.Parent().(*ast.List)

		var indent strings.Builder

		for p := parent.Parent(); p != nil; p = p.Parent() {
			if _, ok := p.(*ast.ListItem); ok {
				indent.WriteString("  ")
			}
		}

		if parent.IsOrdered() {
			idx := 1

			for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
				if c == node {
					break
				}
				idx++
			}
			fmt.Fprintf(w, "%s%s%d.%s ", indent.String(), r.dim(), parent.Start+idx-1, ansi.Reset)
		} else {
			fmt.Fprintf(w, "%s%s•%s ", indent.String(), r.dim(), ansi.Reset)
		}
	} else {
		w.WriteString("\n")
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderThematicBreak(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if node.PreviousSibling() != nil {
			w.WriteString("\n")
		}
		fmt.Fprintf(w, "%s%s%s\n", r.dim(), strings.Repeat("─", 40), ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderHTMLBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.HTMLBlock)
		lines := n.Lines()

		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			w.WriteString(sanitize(string(line.Value(source))))
		}
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderText(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.Text)
		segment := n.Segment
		w.WriteString(sanitize(string(segment.Value(source))))

		if n.HardLineBreak() || n.SoftLineBreak() {
			w.WriteString("\n")
		}
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderString(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.String)
		w.WriteString(sanitize(string(n.Value)))
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderCodeSpan(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString(ansi.Fg(r.theme.Cyan))
	} else {
		w.WriteString(ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderEmphasis(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Emphasis)

	if entering {
		if n.Level == 2 {
			w.WriteString(ansi.Bold)
		} else {
			w.WriteString(ansi.Italic)
		}
	} else {
		w.WriteString(ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Link)

	if entering {
		w.WriteString(ansi.Fg(r.theme.Cyan))
	} else {
		fmt.Fprintf(w, "%s %s(%s)%s", ansi.Reset, r.dim(), sanitize(string(n.Destination)), ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderAutoLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.AutoLink)

	if entering {
		fmt.Fprintf(w, "%s%s%s", ansi.Fg(r.theme.Cyan), sanitize(string(n.URL(source))), ansi.Reset)
	}

	return ast.WalkSkipChildren, nil
}

func (r *ANSIRenderer) renderImage(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Image)

	if entering {
		fmt.Fprintf(w, "%s[image] ", r.dim())
	} else {
		fmt.Fprintf(w, "(%s)%s", sanitize(string(n.Destination)), ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.RawHTML)
		segments := n.Segments

		for i := 0; i < segments.Len(); i++ {
			segment := segments.At(i)
			w.WriteString(sanitize(string(segment.Value(source))))
		}
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderTable(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		w.WriteString("\n")

		return ast.WalkContinue, nil
	}

	if node.PreviousSibling() != nil {
		w.WriteString("\n")
	}

	table := node.(*east.Table)
	var rows [][]string
	var isHeader []bool

	for row := node.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		_, header := row.(*east.TableHeader)
		isHeader = append(isHeader, header)

		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			var cellText strings.Builder

			for child := cell.FirstChild(); child != nil; child = child.NextSibling() {
				if text, ok := child.(*ast.Text); ok {
					cellText.Write(text.Segment.Value(source))
				} else if str, ok := child.(*ast.String); ok {
					cellText.Write(str.Value)
				} else {
					for c := child.FirstChild(); c != nil; c = c.NextSibling() {
						if t, ok := c.(*ast.Text); ok {
							cellText.Write(t.Segment.Value(source))
						}
					}
				}
			}
			cells = append(cells, cellText.String())
		}
		rows = append(rows, cells)
	}

	colWidths := make([]int, len(table.Alignments))

	for _, row := range rows {
		for i, cell := range row {
			cellWidth := visibleLen(cell)

			if i < len(colWidths) && cellWidth > colWidths[i] {
				colWidths[i] = cellWidth
			}
		}
	}

	for rowIdx, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprintf(w, "%s│%s", r.dim(), ansi.Reset)
			}

			escaped := sanitize(cell)
			w.WriteString(" ")

			if isHeader[rowIdx] {
				fmt.Fprintf(w, "%s%s%s", ansi.Bold, escaped, ansi.Reset)
			} else {
				w.WriteString(escaped)
			}

			if i < len(colWidths) {
				padding := colWidths[i] - visibleLen(cell) + 1

				for range padding {
					w.WriteString(" ")
				}
			}
		}

		w.WriteString("\n")

		if isHeader[rowIdx] {
			for i, width := range colWidths {
				if i > 0 {
					fmt.Fprintf(w, "%s┼%s", r.dim(), ansi.Reset)
				}

				fmt.Fprintf(w, "%s%s%s", r.dim(), strings.Repeat("─", width+2), ansi.Reset)
			}

			w.WriteString("\n")
		}
	}

	return ast.WalkSkipChildren, nil
}

func (r *ANSIRenderer) renderTableHeader(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderTableRow(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderTableCell(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderStrikethrough(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString(ansi.Strike)
	} else {
		w.WriteString(ansi.Reset)
	}

	return ast.WalkContinue, nil
}

func (r *ANSIRenderer) renderTaskCheckBox(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*east.TaskCheckBox)

		if n.IsChecked {
			fmt.Fprintf(w, "%s✔%s ", ansi.Fg(r.theme.Green), ansi.Reset)
		} else {
			fmt.Fprintf(w, "%s□%s ", r.dim(), ansi.Reset)
		}
	}

	return ast.WalkContinue, nil
}
