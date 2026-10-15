package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coregraph "github.com/adrianliechti/wingman-agent/pkg/graph"
)

func TestCodeGraphSearchContentOperation(t *testing.T) {
	root := t.TempDir()
	source := `package sample

// marker outside
func Alpha() {
	println("marker")
	println("marker")
}

func Beta() { println("marker") }
`
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := coregraph.New(root, filepath.Join(t.TempDir(), "graph.json"))
	graphTool := NewTools(engine)[0]

	out, err := graphTool.Execute(context.Background(), map[string]any{
		"operation": "search_content",
		"pattern":   "marker",
		"limit":     float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Showing 1 of 2 containing symbol(s)", "Alpha (function)", "matches 5,6", "More symbols available", "Raw matches outside indexed definitions", "sample.go:3"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("output missing %q:\n%s", want, out.Content)
		}
	}
}

func TestCodeGraphSearchReportsPaginationAndRejectsNegativeOffset(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\nfunc Alpha() {}\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := coregraph.New(root, filepath.Join(t.TempDir(), "graph.json"))
	graphTool := NewTools(engine)[0]

	out, err := graphTool.Execute(context.Background(), map[string]any{
		"operation": "search",
		"limit":     float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Showing 1 of 2 symbol(s)") || !strings.Contains(out.Content, "offset=1") {
		t.Fatalf("pagination metadata missing:\n%s", out.Content)
	}
	_, err = graphTool.Execute(context.Background(), map[string]any{
		"operation": "search",
		"offset":    float64(-1),
	})
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("expected negative offset validation, got %v", err)
	}
}

func TestTraceReturnsEvidenceAndCoverageLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "calls.go"), []byte("package sample\nfunc Start() { Middle() }\nfunc Middle() { End() }\nfunc End() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.go"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	inspect := NewTools(coregraph.New(root, filepath.Join(t.TempDir(), "graph.json")))[0]
	out, err := inspect.Execute(t.Context(), map[string]any{"operation": "trace", "symbol": "Start", "target": "End"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Start → Middle → End", "calls.go:2: func Start()", "calls.go:3: func Middle()", "Coverage incomplete: 1", "empty.go (empty)", "Edges resolved by name"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("missing %q: %s", want, out.Content)
		}
	}
	out, err = inspect.Execute(t.Context(), map[string]any{"operation": "trace", "symbol": "Start", "target": "End", "depth": 1})
	if err != nil || !strings.Contains(out.Content, "INCOMPLETE") {
		t.Fatalf("silent truncation: %s %v", out.Content, err)
	}
}
