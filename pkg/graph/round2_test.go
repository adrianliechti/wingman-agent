package graph

import (
	"context"
	"path/filepath"
	"testing"
)

type fakeResolver struct {
	file string
	line int
}

func (f fakeResolver) ResolveCall(ctx context.Context, file string, line, col int) (string, int, bool) {
	return f.file, f.line, true
}

func TestDeadCode(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}

	dead, err := e.DeadCode(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, n := range dead {
		names[n.Name] = true
	}
	if !names["speak"] {
		t.Fatalf("expected speak (0 callers) in dead code, got %v", names)
	}
	for _, alive := range []string{"greet", "format", "noise", "main"} {
		if names[alive] {
			t.Fatalf("%s should not be reported as dead", alive)
		}
	}
}

func TestAmbiguousResolutionFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc run() {\n\ttarget()\n}\n\nfunc target() {\n}\n")
	writeFile(t, root, "other.go", "package main\n\nfunc target() {\n}\n")
	ctx := context.Background()

	e := New(root, filepath.Join(t.TempDir(), "g.json"))
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := e.Trace(ctx, "run", "target", false, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 2 {
		t.Fatalf("expected 2 ambiguous paths without resolver, got %d", len(res.Paths))
	}
	for _, p := range res.Paths {
		if len(p.Via) == 0 || p.Via[len(p.Via)-1] != ViaAmbiguous {
			t.Fatalf("expected ambiguous provenance, got %v", p.Via)
		}
	}
}

func TestResolverDisambiguates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc run() {\n\ttarget()\n}\n\nfunc target() {\n}\n")
	writeFile(t, root, "other.go", "package main\n\nfunc target() {\n}\n")
	ctx := context.Background()

	e := New(root, filepath.Join(t.TempDir(), "g.json"), WithResolver(fakeResolver{file: "other.go", line: 3}))
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := e.Trace(ctx, "run", "target", false, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("expected 1 resolved path, got %d", len(res.Paths))
	}
	p := res.Paths[0]
	last := p.Nodes[len(p.Nodes)-1]
	if last.File != "other.go" {
		t.Fatalf("expected resolution to other.go, got %s", last.File)
	}
	if p.Via[len(p.Via)-1] != ViaLSP {
		t.Fatalf("expected lsp provenance, got %v", p.Via)
	}
}

func TestAffectedNodes(t *testing.T) {
	g := &Graph{Nodes: []*Node{
		{ID: "a", Name: "A", File: "x.go", StartLine: 1, EndLine: 5, Kind: KindFunction},
		{ID: "b", Name: "B", File: "x.go", StartLine: 7, EndLine: 12, Kind: KindFunction},
	}}
	g.build()

	res := affectedNodes(g, []fileChange{{Path: "x.go", Kind: ChangeModified, Lines: []int{8}}})
	if len(res.Files) != 1 {
		t.Fatalf("expected 1 changed file, got %d", len(res.Files))
	}
	if len(res.Files[0].Nodes) != 1 || res.Files[0].Nodes[0].Name != "B" {
		t.Fatalf("expected only B affected by line 8, got %+v", res.Files[0].Nodes)
	}
}

func TestChangedNewLines(t *testing.T) {
	old := "a\nb\nc\n"
	updated := "a\nB\nc\nd\n"

	set := map[int]bool{}
	for _, l := range changedNewLines(old, updated) {
		set[l] = true
	}
	if !set[2] || !set[4] {
		t.Fatalf("expected lines 2 and 4 changed, got %v", set)
	}
}

func TestArchitectureLayersModules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}
	arch, err := e.Architecture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(arch.Layers) == 0 || len(arch.Modules) == 0 {
		t.Fatalf("expected layers and modules, got %+v", arch)
	}
}
