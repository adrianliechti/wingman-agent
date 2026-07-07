package graph

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIsTestFile(t *testing.T) {
	cases := map[string]bool{
		"foo_test.go":         true,
		"pkg/foo_test.go":     true,
		"foo.go":              false,
		"test_foo.py":         true,
		"tests/helpers.py":    true,
		"src/foo.test.ts":     true,
		"src/foo.spec.js":     true,
		"src/__tests__/a.tsx": true,
		"internal/spec/x.go":  true,
		"contestant.go":       false,
		"latest.go":           false,
	}
	for p, want := range cases {
		if got := isTestFile(p); got != want {
			t.Errorf("isTestFile(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestTestsOperation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "calc.go", `package calc

func Add(a, b int) int { return a + b }
`)
	writeFile(t, root, "calc_test.go", `package calc

import "testing"

func TestAdd(t *testing.T) {
	Add(1, 2)
}
`)

	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	ctx := context.Background()
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := e.Tests(ctx, "Add", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TestedBy) != 1 || res.TestedBy[0].Name != "TestAdd" {
		t.Fatalf("Add tested by = %+v", res.TestedBy)
	}

	res2, err := e.Tests(ctx, "TestAdd", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Covers) != 1 || res2.Covers[0].Name != "Add" {
		t.Fatalf("TestAdd covers = %+v", res2.Covers)
	}
}
