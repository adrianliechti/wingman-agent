package todo

import (
	"context"
	"strings"
	"testing"
)

func TestTodoWriteReplacesSessionList(t *testing.T) {
	tool := Tools()[0]

	result, err := tool.Execute(context.Background(), map[string]any{
		"todos": []any{
			map[string]any{"content": "Inspect code", "status": "completed"},
			map[string]any{"content": "Add tests", "status": "in_progress"},
			map[string]any{"content": "Run tests", "status": "pending"},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"[x] Inspect code", "[~] Add tests", "[ ] Run tests"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in result, got: %s", want, result)
		}
	}

	result, err = tool.Execute(context.Background(), map[string]any{
		"todos": []any{
			map[string]any{"content": "Done", "status": "completed"},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error replacing list: %v", err)
	}

	if strings.Contains(result, "Inspect code") || !strings.Contains(result, "[x] Done") {
		t.Errorf("expected replacement list only, got: %s", result)
	}
}

func TestTodoWriteValidation(t *testing.T) {
	tool := Tools()[0]

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "missing todos",
			args: map[string]any{},
			want: "todos is required",
		},
		{
			name: "missing content",
			args: map[string]any{"todos": []any{map[string]any{"status": "pending"}}},
			want: "content is required",
		},
		{
			name: "invalid status",
			args: map[string]any{"todos": []any{map[string]any{"content": "Task", "status": "blocked"}}},
			want: "status must be pending, in_progress, or completed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got: %v", tt.want, err)
			}
		})
	}
}
