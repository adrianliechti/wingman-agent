package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func Tools() []tool.Tool {
	description := strings.Join([]string{
		"Search the web. Returns titles, URLs, and content snippets.",
		"- Use for current events, recent docs, library versions — anything time-sensitive.",
		"- Skip for stable facts (language syntax, settled APIs); answer from training.",
	}, "\n")

	return []tool.Tool{{
		Name:        "search_online",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query."},
			},

			"required": []string{"query"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			query, ok := args["query"].(string)

			if !ok || query == "" {
				return "", fmt.Errorf("query is required")
			}

			wingmanURL := os.Getenv("WINGMAN_URL")

			if wingmanURL == "" {
				return "", fmt.Errorf("search is not available: WINGMAN_URL is not configured")
			}

			return searchWingman(ctx, wingmanURL, os.Getenv("WINGMAN_TOKEN"), query)
		},
	}}
}

func searchWingman(ctx context.Context, baseURL, token, query string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/search"

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("query", query); err != nil {
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
		return "", fmt.Errorf("search API returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)

	if err != nil {
		return "", err
	}

	var structured struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}

	if err := json.Unmarshal(data, &structured); err == nil && len(structured.Results) > 0 {
		var sb strings.Builder

		for i, r := range structured.Results {
			fmt.Fprintf(&sb, "## %d. %s\n", i+1, r.Title)

			if r.URL != "" {
				fmt.Fprintf(&sb, "URL: %s\n", r.URL)
			}

			fmt.Fprintf(&sb, "%s\n\n", r.Content)
		}

		return sb.String(), nil
	}

	result := strings.TrimSpace(string(data))

	if result == "" {
		return "", fmt.Errorf("empty response from search API")
	}

	return result, nil
}
