package markdown

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// Options supplies the available content width and workspace for file links.
type Options struct {
	Width     int
	Directory string
}

// Render converts markdown to ANSI-styled terminal text. An unterminated
// trailing code fence (mid-stream) is still highlighted live.
func Render(text string, options ...Options) string {
	var settings Options
	if len(options) > 0 {
		settings = options[0]
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

	// Goldmark also parses unterminated fences, keeping streamed blocks on the
	// same rendering path (including list nesting) as completed blocks.
	if err := md.Convert([]byte(text), &buf); err != nil {
		return text
	}

	return buf.String()
}
