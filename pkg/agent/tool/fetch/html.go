package fetch

import (
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

func htmlToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}

	title := pageTitle(doc)

	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			table.NewTablePlugin(),
			strikethrough.NewStrikethroughPlugin(),
		),
	)

	for _, tag := range []string{"svg", "template", "object", "embed"} {
		conv.Register.TagType(tag, converter.TagTypeRemove, converter.PriorityStandard)
	}

	out, err := conv.ConvertNode(doc)
	if err != nil {
		return src
	}

	text := strings.TrimSpace(string(out))
	if title != "" && !strings.HasPrefix(text, "# ") {
		text = "# " + title + "\n\n" + text
	}
	return text
}

func pageTitle(doc *html.Node) string {
	var title string
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "title" {
			if n.FirstChild != nil {
				title = strings.TrimSpace(n.FirstChild.Data)
			}
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(doc)
	return title
}
