package search

import (
	"context"
	"testing"
)

func TestSearchTool(t *testing.T) {
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
	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{})

	if err == nil {
		t.Error("expected error for missing query parameter")
	}
}

func TestSearchToolShortQuery(t *testing.T) {
	searchTool := Tools()[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{
		"query": "x",
	})

	if err == nil {
		t.Error("expected error for short query parameter")
	}
}

func TestSearchToolCannotMixDomainFilters(t *testing.T) {
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

func TestSearchToolNoWingmanURL(t *testing.T) {
	searchTool := Tools()[0]

	t.Setenv("WINGMAN_URL", "")

	_, err := searchTool.Execute(context.Background(), map[string]any{
		"query": "golang programming",
	})

	if err == nil {
		t.Error("expected error when WINGMAN_URL is not set")
	}
}
