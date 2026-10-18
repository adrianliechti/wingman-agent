package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	coregraph "github.com/adrianliechti/wingman-agent/pkg/graph"
)

func TestInvalidGraphArgumentsDoNotBuildIndex(t *testing.T) {
	engine := coregraph.New(t.TempDir(), "")
	inspect := NewTools(engine)[0]
	for _, args := range []map[string]any{
		{"operation": "search", "query": false},
		{"operation": "search_content", "pattern": "needle", "regex": "true"},
		{"operation": "search_content", "pattern": "needle", "ignore_case": nil},
		{"operation": "trace", "symbol": "main", "direction": "invalid"},
		{"operation": "search", "kind": "invalid"},
		{"operation": "deps", "file": "pkg", "depth": 1.5},
		{"operation": "trace", "symbol": "main", "depth": 33},
		{"operation": "dead_code", "limit": -1},
		{"operation": "co_changes", "file": "main.go", "limit": "10"},
		{"operation": "find_similar", "symbol": "main", "limit": 1001},
		{"operation": "search", "offset": nil},
	} {
		if _, err := inspect.Execute(t.Context(), args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if engine.Status().Indexed {
		t.Fatal("invalid arguments built an index")
	}
}

func TestCancelledGraphStatusDoesNotSucceed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	inspect := NewTools(coregraph.New(t.TempDir(), ""))[0]
	if _, err := inspect.Execute(ctx, map[string]any{"operation": "status"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled status: %v", err)
	}
}

func TestIncompleteContentSearchIsVisibleWithoutMatches(t *testing.T) {
	result := coregraph.ContentSearchResult{SkippedFiles: []coregraph.CoverageIssue{{File: "large.go", Reason: "scan incomplete"}}}
	output := formatContentSearch(result)
	if !strings.Contains(output, "INCOMPLETE") || !strings.Contains(output, "large.go") || !strings.Contains(output, "scanned content") {
		t.Fatalf("incomplete scan presented as exhaustive: %s", output)
	}
}
