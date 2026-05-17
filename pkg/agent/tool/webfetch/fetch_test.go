package webfetch_test

import (
	"context"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/webfetch"
)

func TestFetchTool(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	fetchTool := Tools()[0]

	if fetchTool.Name != "web_fetch" {
		t.Errorf("expected name 'web_fetch', got %q", fetchTool.Name)
	}

	if fetchTool.Description == "" {
		t.Error("expected non-empty description")
	}

	if fetchTool.Parameters == nil {
		t.Error("expected non-nil parameters")
	}

	if fetchTool.Execute == nil {
		t.Error("expected non-nil execute function")
	}
}

func TestFetchToolMissingPrompt(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	fetchTool := Tools()[0]

	_, err := fetchTool.Execute(context.Background(), map[string]any{
		"url": "https://example.com/docs",
	})

	if err == nil {
		t.Error("expected error for missing prompt parameter")
	}
}

func TestFetchToolNotRegisteredWithoutWingmanURL(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")

	if tools := Tools(); len(tools) != 0 {
		t.Fatalf("Tools() returned %d tools, want 0", len(tools))
	}
}

func TestFetchToolValidatesAndNormalizesURL(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")

	fetchTool := Tools()[0]

	tests := []struct {
		name string
		url  string
	}{
		{name: "relative rejected", url: "example.com/docs"},
		{name: "ftp rejected", url: "ftp://example.com/docs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fetchTool.Execute(context.Background(), map[string]any{
				"url":    tt.url,
				"prompt": "extract",
			})
			if err == nil {
				t.Fatalf("expected URL validation error")
			}
		})
	}
}
