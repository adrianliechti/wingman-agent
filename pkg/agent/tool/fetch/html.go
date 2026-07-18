package fetch

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var skippedElements = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"svg":      true,
	"iframe":   true,
	"object":   true,
	"embed":    true,
}

var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"div": true, "dl": true, "dd": true, "dt": true, "fieldset": true,
	"figure": true, "figcaption": true, "footer": true, "form": true,
	"header": true, "hr": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tbody": true,
	"thead": true, "tr": true, "ul": true,
}

func htmlToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}

	var b strings.Builder
	var title string
	renderNode(&b, doc, &title)

	text := collapseWhitespace(b.String())
	if title != "" && !strings.HasPrefix(text, "# ") {
		text = "# " + title + "\n\n" + text
	}
	return text
}

func renderNode(b *strings.Builder, n *html.Node, title *string) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return

	case html.ElementNode:
		tag := n.Data

		if skippedElements[tag] {
			return
		}

		switch {
		case tag == "title":
			if *title == "" {
				*title = strings.TrimSpace(textContent(n))
			}
			return

		case tag == "br":
			b.WriteString("\n")
			return

		case len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
			b.WriteString("\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " ")
			renderChildren(b, n, title)
			b.WriteString("\n\n")
			return

		case tag == "li":
			b.WriteString("\n- ")
			renderChildren(b, n, title)
			return

		case tag == "a":
			text := strings.TrimSpace(textContent(n))
			href := attrValue(n, "href")
			if text == "" {
				return
			}
			if href == "" || href == text || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
				b.WriteString(text)
				return
			}
			fmt.Fprintf(b, "[%s](%s)", text, href)
			return

		case tag == "td", tag == "th":
			renderChildren(b, n, title)
			b.WriteString(" | ")
			return

		case blockElements[tag]:
			b.WriteString("\n")
			renderChildren(b, n, title)
			b.WriteString("\n")
			return
		}
	}

	renderChildren(b, n, title)
}

func renderChildren(b *strings.Builder, n *html.Node, title *string) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(b, c, title)
	}
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func attrValue(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

var (
	spaceRuns   = regexp.MustCompile(`[ \t]+`)
	newlineRuns = regexp.MustCompile(`\n{3,}`)
)

func collapseWhitespace(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spaceRuns.ReplaceAllString(line, " "))
	}
	return strings.TrimSpace(newlineRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
