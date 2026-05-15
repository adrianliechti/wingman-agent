package todo

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type item struct {
	Content string
	Status  string
}

func Tools() []tool.Tool {
	var mu sync.Mutex
	var todos []item

	return []tool.Tool{{
		Name:   "todo_write",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Replace the current session todo list. Use for complex multi-step coding work.",
			"- Store only task progress for this conversation; this does not write files.",
			"- Exactly one todo should be in_progress while actively working.",
			"- Skip for single-step or purely conversational tasks.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "Complete replacement todo list.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "Task description."},
							"status": map[string]any{
								"type":        "string",
								"description": "Task state.",
								"enum":        []string{"pending", "in_progress", "completed"},
							},
						},
						"required": []string{"content", "status"},
					},
				},
			},
			"required": []string{"todos"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			next, err := parseTodos(args)
			if err != nil {
				return "", err
			}

			mu.Lock()
			todos = next
			output := formatTodos(todos)
			mu.Unlock()

			return output, nil
		},
	}}
}

func parseTodos(args map[string]any) ([]item, error) {
	raw, ok := args["todos"].([]any)
	if !ok {
		return nil, fmt.Errorf("todos is required")
	}

	todos := make([]item, 0, len(raw))
	for i, value := range raw {
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("todos[%d] must be an object", i)
		}

		content, ok := obj["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("todos[%d].content is required", i)
		}

		status, ok := obj["status"].(string)
		if !ok || !validStatus(status) {
			return nil, fmt.Errorf("todos[%d].status must be pending, in_progress, or completed", i)
		}

		todos = append(todos, item{Content: content, Status: status})
	}

	return todos, nil
}

func validStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed":
		return true
	default:
		return false
	}
}

func formatTodos(todos []item) string {
	if len(todos) == 0 {
		return "Todo list cleared."
	}

	var b strings.Builder
	b.WriteString("Todo list updated:\n")

	for _, todo := range todos {
		marker := " "
		switch todo.Status {
		case "in_progress":
			marker = "~"
		case "completed":
			marker = "x"
		}

		fmt.Fprintf(&b, "- [%s] %s\n", marker, todo.Content)
	}

	return strings.TrimRight(b.String(), "\n")
}
