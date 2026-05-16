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
			"- Use proactively for tasks with 3+ meaningful steps, multiple requested changes, plan-mode investigations, or when the user explicitly asks for a todo list.",
			"- At most one todo may be in_progress at a time. Mark completed immediately after finishing; do not batch all completions at the end.",
			"- Do not mark a todo completed while tests/checks are failing, implementation is partial, or you are blocked. Add or keep a pending item for the blocker instead.",
			"- When every todo is completed, the visible list is cleared; the completed update is still accepted.",
			"- Skip for single-step, trivial, or purely conversational tasks.",
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
			visible := next
			if allCompleted(next) {
				visible = nil
			}

			mu.Lock()
			todos = visible
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
	inProgress := 0
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
		if status == "in_progress" {
			inProgress++
			if inProgress > 1 {
				return nil, fmt.Errorf("only one todo may be in_progress")
			}
		}

		todos = append(todos, item{Content: content, Status: status})
	}

	return todos, nil
}

func allCompleted(todos []item) bool {
	if len(todos) == 0 {
		return false
	}

	for _, todo := range todos {
		if todo.Status != "completed" {
			return false
		}
	}

	return true
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
