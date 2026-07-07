package todo

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

var statusMarkers = map[string]string{
	"pending":     "[ ]",
	"in_progress": "[>]",
	"completed":   "[x]",
}

func Tools() []tool.Tool {
	description := strings.Join([]string{
		"Update your task list for the current session. Each call replaces the whole list.",
		"- Use for multi-step work (3+ distinct steps) or when the user gives several tasks. Skip it for single straightforward tasks — writing the list would cost more than it guides.",
		"- Keep exactly one item in_progress at a time. Mark items completed immediately when they are done; don't batch completions.",
		"- Add newly discovered steps as they come up, and remove items that became irrelevant instead of leaving them pending.",
	}, "\n")

	return []tool.Tool{{
		Name:        "todo",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{
					"type":        "array",
					"description": "The full task list in execution order; replaces the previous list.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "Short imperative description of the step."},
							"status": map[string]any{
								"type": "string",
								"enum": []string{"pending", "in_progress", "completed"},
							},
						},
						"required":             []string{"content", "status"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"items"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			raw, ok := args["items"].([]any)
			if !ok {
				return "", fmt.Errorf("items must be an array")
			}

			if len(raw) == 0 {
				return "Todo list cleared.", nil
			}

			var b strings.Builder
			completed := 0

			for i, item := range raw {
				entry, ok := item.(map[string]any)
				if !ok {
					return "", fmt.Errorf("items[%d] must be an object with content and status", i)
				}

				content, _ := entry["content"].(string)
				if strings.TrimSpace(content) == "" {
					return "", fmt.Errorf("items[%d].content is required", i)
				}

				status, _ := entry["status"].(string)
				marker, ok := statusMarkers[status]
				if !ok {
					return "", fmt.Errorf("items[%d].status must be pending, in_progress, or completed", i)
				}
				if status == "completed" {
					completed++
				}

				fmt.Fprintf(&b, "%s %s\n", marker, strings.TrimSpace(content))
			}

			fmt.Fprintf(&b, "(%d/%d completed)", completed, len(raw))

			return b.String(), nil
		},
	}}
}
