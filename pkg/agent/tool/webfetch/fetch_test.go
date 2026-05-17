package webfetch

import (
	"context"
	"testing"
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

func TestNormalizeFetchURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "https", raw: "https://example.com/docs", want: "https://example.com/docs"},
		{name: "http upgraded", raw: "http://example.com/docs", want: "https://example.com/docs"},
		{name: "trims", raw: " https://example.com/docs ", want: "https://example.com/docs"},
		{name: "relative rejected", raw: "example.com/docs", wantErr: true},
		{name: "ftp rejected", raw: "ftp://example.com/docs", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeFetchURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeFetchURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
