package markdown

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type CopyTarget struct {
	Label string
	Text  string
}

// CopyTargets extracts exact code payloads and readable blockquotes from the
// source AST, independent of terminal wrapping and syntax highlighting.
func CopyTargets(source string) []CopyTarget {
	data := []byte(source)
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(data))
	var targets []CopyTarget
	codeIndex, quoteIndex := 0, 0
	ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			codeIndex++
			label := fmt.Sprintf("Code block %d", codeIndex)
			if block, ok := n.(*ast.FencedCodeBlock); ok && len(block.Language(data)) > 0 {
				label += " · " + sanitize(string(block.Language(data)))
			}
			targets = append(targets, CopyTarget{Label: label, Text: string(node.Lines().Value(data))})
			return ast.WalkSkipChildren, nil
		case *ast.Blockquote:
			for parent := node.Parent(); parent != nil; parent = parent.Parent() {
				if parent.Kind() == ast.KindBlockquote {
					return ast.WalkContinue, nil
				}
			}
			if quote := copyQuote(node, data); quote != "" {
				quoteIndex++
				targets = append(targets, CopyTarget{Label: fmt.Sprintf("Blockquote %d", quoteIndex), Text: quote})
			}
		}
		return ast.WalkContinue, nil
	})
	return targets
}

func copyQuote(node ast.Node, source []byte) string {
	var out strings.Builder
	ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch n := n.(type) {
		case *ast.Text:
			if entering {
				out.Write(n.Segment.Value(source))
				if n.SoftLineBreak() || n.HardLineBreak() {
					out.WriteByte('\n')
				}
			}
		case *ast.String:
			if entering {
				out.Write(n.Value)
			}
		case *ast.Paragraph:
			if !entering {
				out.WriteString("\n\n")
			}
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(out.String())
}
