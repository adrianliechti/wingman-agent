package webfetch

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/wingman"
)

func Tools() []tool.Tool {
	client, err := wingman.FromEnv()
	if err != nil {
		return nil
	}

	description := strings.Join([]string{
		"Fetch a fully formed URL, convert HTML to markdown, and process it with the supplied prompt.",
		"- Fails for authenticated/private URLs (Google Docs, Confluence, Jira, GitHub private pages); prefer an authenticated MCP tool when available.",
		"- HTTP URLs are upgraded to HTTPS.",
		"- The prompt should say exactly what to extract from the page.",
		"- Results may be summarized if content is large; repeated fetches use a 15-minute cache.",
		"- If redirected to a different host, call `web_fetch` again with the provided redirect URL.",
		"- For GitHub URLs, prefer `gh` via shell (`gh pr view`, `gh issue view`, `gh api`).",
	}, "\n")

	return []tool.Tool{{
		Name:        "web_fetch",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"url":    map[string]any{"type": "string", "description": "The URL to fetch content from"},
				"prompt": map[string]any{"type": "string", "description": "The prompt to run on the fetched content"},
			},

			"required":             []string{"url", "prompt"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			urlStr, ok := args["url"].(string)

			if !ok || urlStr == "" {
				return "", fmt.Errorf("url is required")
			}

			prompt, ok := args["prompt"].(string)
			prompt = strings.TrimSpace(prompt)

			if !ok || prompt == "" {
				return "", fmt.Errorf("prompt is required")
			}

			normalizedURL, err := normalizeFetchURL(urlStr)
			if err != nil {
				return "", err
			}

			return extractWingman(ctx, client, normalizedURL, prompt)
		},
	}}
}

func normalizeFetchURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("url must be a fully formed http(s) URL")
	}

	switch parsed.Scheme {
	case "https":
		return parsed.String(), nil
	case "http":
		parsed.Scheme = "https"
		return parsed.String(), nil
	default:
		return "", fmt.Errorf("url must use http or https")
	}
}

func extractWingman(ctx context.Context, client *wingman.Client, urlStr, prompt string) (string, error) {
	content, truncated, err := client.Fetch(ctx, urlStr, prompt)
	if err != nil {
		return "", err
	}

	if truncated {
		content += "\n\n[Content truncated at 100KB]"
	}

	return content, nil
}
