package websearch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/wingman"
)

func Tools() []tool.Tool {
	client, err := wingman.FromEnv()
	if err != nil {
		return nil
	}

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

			return searchWingman(ctx, client, query, allowedDomains, blockedDomains)
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

func searchWingman(ctx context.Context, client *wingman.Client, query string, allowedDomains, blockedDomains []string) (string, error) {
	results, text, err := client.Search(ctx, query, allowedDomains, blockedDomains)
	if err != nil {
		return "", err
	}

	if len(results) > 0 {
		var sb strings.Builder

		fmt.Fprintf(&sb, "Web search results for query: %q\n\n", query)

		for i, r := range results {
			fmt.Fprintf(&sb, "## %d. %s\n", i+1, r.Title)

			if r.URL != "" {
				fmt.Fprintf(&sb, "URL: [%s](%s)\n", r.Title, r.URL)
			}

			fmt.Fprintf(&sb, "%s\n\n", r.Content)
		}

		fmt.Fprintf(&sb, "REMINDER: You MUST include the sources above in your response to the user using markdown hyperlinks.\n")

		return sb.String(), nil
	}

	return text, nil
}
