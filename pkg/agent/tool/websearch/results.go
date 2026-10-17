package websearch

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

type result struct {
	title   string
	url     string
	snippet string
}

// A result's title and snippet must come from the same container. Skipped ads
// or malformed links must never donate their snippets to preceding results.
func parseResults(page io.Reader) ([]result, error) {
	doc, err := html.Parse(page)
	if err != nil {
		return nil, fmt.Errorf("web search: parse response: %w", err)
	}
	var results []result
	seen := make(map[string]bool)
	var noResults, blocked bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if attr(n, "id") == "challenge-form" || hasClass(n, "anomaly-modal") || hasClass(n, "anomaly-modal__modal") {
				blocked = true
			}
			if hasClass(n, "no-results") || hasClass(n, "result--no-result") {
				noResults = true
			}
			if hasClass(n, "result") && !hasClass(n, "result--ad") && !hasClass(n, "result--no-result") {
				r := parseResult(n)
				if r.title != "" && r.url != "" && !seen[r.url] {
					seen[r.url] = true
					results = append(results, r)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if blocked {
		return nil, fmt.Errorf("web search: DuckDuckGo returned a verification challenge; search results are unavailable")
	}
	if len(results) == 0 && !noResults {
		return nil, fmt.Errorf("web search: unrecognized response from DuckDuckGo; search results could not be read")
	}
	return results, nil
}

func parseResult(root *html.Node) result {
	var r result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n != root && hasClass(n, "result") {
			return
		}
		if n.Type == html.ElementNode {
			if n.Data == "a" && hasClass(n, "result__a") && r.title == "" {
				r.title, r.url = nodeText(n), resolveResultURL(attr(n, "href"))
			}
			if hasClass(n, "result__snippet") && r.snippet == "" {
				r.snippet = nodeText(n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return r
}

func resolveResultURL(href string) string {
	// Bound redirect unwrapping instead of recursing on provider-controlled URLs.
	for range 10 {
		href = strings.TrimSpace(href)
		if href == "" || !utf8.ValidString(href) || strings.IndexFunc(href, unicode.IsControl) >= 0 {
			return ""
		}
		if strings.HasPrefix(href, "//") {
			href = "https:" + href
		} else if strings.HasPrefix(href, "/l/") || strings.HasPrefix(href, "/l?") || href == "/l" {
			href = "https://duckduckgo.com" + href
		}
		parsed, err := url.Parse(href)
		if err != nil {
			return ""
		}
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil {
			return ""
		}
		if port := parsed.Port(); port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return ""
			}
		}
		if isSearchHost(parsed.Hostname()) {
			switch parsed.Path {
			case "/y.js": // Advertising redirects are not organic results.
				return ""
			case "/l/", "/l":
				href = parsed.Query().Get("uddg")
				continue
			}
		}
		out := parsed.String()
		if !utf8.ValidString(out) || strings.IndexFunc(out, unicode.IsControl) >= 0 {
			return ""
		}
		return out
	}
	return ""
}

func isSearchHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")
}

func formatResults(results []result) (tool.Result, error) {
	var b strings.Builder
	shown := 0
	for _, r := range results {
		entry := fmt.Sprintf("%d. %s\n   %s\n", shown+1, text.TruncateHead(r.title, 512), r.url)
		if r.snippet != "" {
			entry += fmt.Sprintf("   %s\n", text.TruncateHead(r.snippet, 2048))
		}
		// Keep URLs intact. Reserve space for an omission notice, and try later
		// results if one unusually long URL cannot fit.
		if b.Len()+len(entry) > tool.MaxInlineResultBytes-256 {
			continue
		}
		b.WriteString(entry)
		shown++
	}
	if shown == 0 {
		return tool.Result{}, fmt.Errorf("web search: result URLs exceed the output size limit; narrow the query")
	}
	if shown < len(results) {
		fmt.Fprintf(&b, "\n[%d results omitted to fit the output size limit.]\n", len(results)-shown)
	}
	return tool.Text(strings.TrimRight(b.String(), "\n")), nil
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "script" || n.Data == "style" || n.Data == "template" {
				return
			}
			if n.Data == "br" {
				b.WriteByte(' ')
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(b.String(), "\uFFFD"))
	return strings.Join(strings.Fields(cleaned), " ")
}
