package todo

import (
	"context"
	"strings"
	"testing"
)

func TestTodoRendersChecklist(t *testing.T) {
	tl := Tools()[0]

	result, err := tl.Execute(context.Background(), map[string]any{
		"items": []any{
			map[string]any{"content": "read the code", "status": "completed"},
			map[string]any{"content": "apply the fix", "status": "in_progress"},
			map[string]any{"content": "run the tests", "status": "pending"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"[x] read the code", "[>] apply the fix", "[ ] run the tests", "(1/3 completed)"} {
		if !strings.Contains(result, want) {
			t.Errorf("result missing %q:\n%s", want, result)
		}
	}
}

func TestTodoValidatesInput(t *testing.T) {
	tl := Tools()[0]

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing items", map[string]any{}},
		{"invalid status", map[string]any{"items": []any{map[string]any{"content": "x", "status": "done"}}}},
		{"empty content", map[string]any{"items": []any{map[string]any{"content": " ", "status": "pending"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tl.Execute(context.Background(), tc.args); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTodoEmptyListClears(t *testing.T) {
	tl := Tools()[0]

	result, err := tl.Execute(context.Background(), map[string]any{"items": []any{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "cleared") {
		t.Fatalf("unexpected result: %s", result)
	}
}
