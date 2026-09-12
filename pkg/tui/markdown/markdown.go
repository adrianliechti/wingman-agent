package markdown

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// Options supplies the available content width and workspace for file links.
type Options struct {
	Width     int
	Directory string
}

// Render converts markdown to ANSI-styled terminal text. An unterminated
// trailing code fence (mid-stream) is still highlighted live.
func Render(text string, options ...Options) string {
	t := theme.Default
	var settings Options
	if len(options) > 0 {
		settings = options[0]
	}

	completeText := text
	incompleteCode := ""
	incompleteLang := ""

	backtickCount := strings.Count(text, "```")

	if backtickCount%2 == 1 {
		incompleteCodeBlockRe := regexp.MustCompile("(?s)```([\\w+#.-]*)\\n([^`]*)$")
		matches := incompleteCodeBlockRe.FindStringSubmatchIndex(text)

		if matches != nil {
			incompleteLang = text[matches[2]:matches[3]]
			incompleteCode = text[matches[4]:matches[5]]
			completeText = text[:matches[0]]
		}
	}

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRenderer(
			renderer.NewRenderer(
				renderer.WithNodeRenderers(
					util.Prioritized(NewANSIRenderer(settings), 100),
				),
			),
		),
	)

	var buf bytes.Buffer

	if err := md.Convert([]byte(completeText), &buf); err != nil {
		return text
	}

	result := buf.String()

	if incompleteCode != "" {
		if result != "" && !strings.HasSuffix(result, "\n\n") {
			result += "\n"
		}
		result += formatCodeBlock(incompleteCode, incompleteLang, t)
	}

	return result
}
