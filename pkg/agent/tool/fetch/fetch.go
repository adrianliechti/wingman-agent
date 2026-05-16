package fetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const maxFetchBytes = 100 * 1024

func Tools() []tool.Tool {
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

			wingmanURL := os.Getenv("WINGMAN_URL")

			if wingmanURL == "" {
				return "", fmt.Errorf("web_fetch is not available: WINGMAN_URL is not configured")
			}

			return extractWingman(ctx, wingmanURL, os.Getenv("WINGMAN_TOKEN"), normalizedURL, prompt)
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

func extractWingman(ctx context.Context, baseURL, token, urlStr, prompt string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/extract"

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("url", urlStr); err != nil {
		return "", err
	}

	if err := writer.WriteField("prompt", prompt); err != nil {
		return "", err
	}

	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, &body)

	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("extract API returned HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxFetchBytes+1)

	data, err := io.ReadAll(limited)

	if err != nil {
		return "", err
	}

	content := strings.TrimSpace(string(data))

	if len(data) > maxFetchBytes {
		content = content[:maxFetchBytes] + "\n\n[Content truncated at 100KB]"
	}

	if content == "" {
		return "", fmt.Errorf("empty response from extract API")
	}

	return content, nil
}
