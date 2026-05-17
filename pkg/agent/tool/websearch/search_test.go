package websearch_test

import (
	"context"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/websearch"
)

func TestSearchTool(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	searchTool := Tools()[0]

	if searchTool.Name != "web_search" {
		t.Errorf("expected name 'web_search', got %q", searchTool.Name)
	}

	if searchTool.Description == "" {
		t.Error("expected non-empty description")
	}

	if searchTool.Parameters == nil {
		t.Error("expected non-nil parameters")
	}

	if searchTool.Execute == nil {
		t.Error("expected non-nil execute function")
	}
}

func TestSearchToolMissingQuery(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{})

	if err == nil {
		t.Error("expected error for missing query parameter")
	}
}

func TestSearchToolShortQuery(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{
		"query": "x",
	})

	if err == nil {
		t.Error("expected error for short query parameter")
	}
}

func TestSearchToolCannotMixDomainFilters(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{
		"query":           "golang programming",
		"allowed_domains": []any{"go.dev"},
		"blocked_domains": []any{"example.com"},
	})

	if err == nil {
		t.Error("expected error when both allowed_domains and blocked_domains are set")
	}
}

func TestSearchToolNotRegisteredWithoutWingmanURL(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")

	if tools := Tools(); len(tools) != 0 {
		t.Fatalf("Tools() returned %d tools, want 0", len(tools))
	}
}

func TestSearchToolWhitespaceQueryRejected(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")
	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{
		"query": "   \t\n  ",
	})
	if err == nil {
		t.Fatal("expected whitespace-only query to be rejected")
	}
}

func TestSearchToolRejectsNonStringDomainEntries(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")
	searchTool := Tools()[0]

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "allowed_domains contains number",
			args: map[string]any{"query": "golang", "allowed_domains": []any{"go.dev", 42}},
		},
		{
			name: "allowed_domains not an array",
			args: map[string]any{"query": "golang", "allowed_domains": "go.dev"},
		},
		{
			name: "blocked_domains contains bool",
			args: map[string]any{"query": "golang", "blocked_domains": []any{true}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := searchTool.Execute(context.Background(), tc.args)
			if err == nil {
				t.Fatal("expected type-validation error")
			}
		})
	}
}
