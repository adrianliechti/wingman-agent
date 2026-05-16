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
	currentMonthYear := time.Now().Format("January 2006")

	description := strings.Join([]string{
		"Search the web for current or post-cutoff information.",
		"- Returns search result blocks with markdown links.",
		"- After using this tool, include a `Sources:` section listing relevant result URLs as markdown links.",
		"- Domain filters can include or block specific websites; web search is US-only.",
		fmt.Sprintf("- Current month: %s. Use this year for recent information, documentation, or current events.", currentMonthYear),
	}, "\n")

	return []tool.Tool{{
		Name:        "web_search",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"query":           map[string]any{"type": "string", "description": "The search query to use", "minLength": 2},
				"allowed_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only include search results from these domains"},
				"blocked_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Never include search results from these domains"},
			},

			"required":             []string{"query"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			query, ok := args["query"].(string)

			query = strings.TrimSpace(query)

			if !ok || query == "" {
				return "", fmt.Errorf("query is required")
			}

			if len(query) < 2 {
				return "", fmt.Errorf("query must be at least 2 characters")
			}

			allowedDomains, err := stringSliceArg(args, "allowed_domains")
			if err != nil {
				return "", err
			}

			blockedDomains, err := stringSliceArg(args, "blocked_domains")
			if err != nil {
				return "", err
			}

			if len(allowedDomains) > 0 && len(blockedDomains) > 0 {
				return "", fmt.Errorf("cannot specify both allowed_domains and blocked_domains in the same request")
			}

			wingmanURL := os.Getenv("WINGMAN_URL")

			if wingmanURL == "" {
				return "", fmt.Errorf("web_search is not available: WINGMAN_URL is not configured")
			}

			return searchWingman(ctx, wingmanURL, os.Getenv("WINGMAN_TOKEN"), query, allowedDomains, blockedDomains)
		},
	}}
}

func stringSliceArg(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}

	values, ok := raw.([]any)
	if !ok {
		if strings, ok := raw.([]string); ok {
			return strings, nil
		}
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}

	result := make([]string, 0, len(values))
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		if s = strings.TrimSpace(s); s != "" {
			result = append(result, s)
		}
	}

	return result, nil
}

func searchWingman(ctx context.Context, baseURL, token, query string, allowedDomains, blockedDomains []string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/search"

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("query", query); err != nil {
		return "", err
	}

	if len(allowedDomains) > 0 {
		data, err := json.Marshal(allowedDomains)
		if err != nil {
			return "", err
		}
		if err := writer.WriteField("allowed_domains", string(data)); err != nil {
			return "", err
		}
	}

	if len(blockedDomains) > 0 {
		data, err := json.Marshal(blockedDomains)
		if err != nil {
			return "", err
		}
		if err := writer.WriteField("blocked_domains", string(data)); err != nil {
			return "", err
		}
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

		fmt.Fprintf(&sb, "Web search results for query: %q\n\n", query)

		for i, r := range structured.Results {
			fmt.Fprintf(&sb, "## %d. %s\n", i+1, r.Title)

			if r.URL != "" {
				fmt.Fprintf(&sb, "URL: [%s](%s)\n", r.Title, r.URL)
			}

			fmt.Fprintf(&sb, "%s\n\n", r.Content)
		}

		fmt.Fprintf(&sb, "REMINDER: You MUST include the sources above in your response to the user using markdown hyperlinks.\n")

		return sb.String(), nil
	}

	result := strings.TrimSpace(string(data))

	if result == "" {
		return "", fmt.Errorf("empty response from search API")
	}

	return result, nil
}
