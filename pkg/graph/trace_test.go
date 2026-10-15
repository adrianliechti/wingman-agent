package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTracePreservesMergingPathsCyclesAndCachedEvidence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "calls.go", "package sample\nfunc Start() { Left(); Right() }\nfunc Left() { Join() }\nfunc Right() { Join() }\nfunc Join() { Start(); End() }\nfunc End() {}\n")
	cache := filepath.Join(t.TempDir(), "graph.json")
	var first []TraceEvidence
	for _, engine := range []*Engine{New(root, cache), New(root, cache)} {
		for _, callers := range []bool{false, true} {
			from, to := "Start", "calls.go:End"
			if callers {
				from, to = "End", "Start"
			}
			result, err := engine.Trace(t.Context(), from, to, "", callers, 8)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Paths) != 2 || result.Limited || result.EvidenceOmitted {
				t.Fatalf("incomplete trace: %+v", result)
			}
			branches := map[string]bool{}
			for _, path := range result.Paths {
				if len(path.Nodes) != 4 {
					t.Fatalf("cycle leaked: %+v", path)
				}
				index := 1
				if callers {
					index = 2
				}
				branches[path.Nodes[index].Name] = true
			}
			if !branches["Left"] || !branches["Right"] {
				t.Fatal(branches)
			}
			if len(result.Evidence) != 4 {
				t.Fatalf("evidence not deduplicated: %+v", result.Evidence)
			}
			for _, site := range result.Evidence {
				if site.File != "calls.go" || site.Line < 2 || site.Line > 5 || !strings.Contains(site.Source, "()") {
					t.Fatalf("bad call-site evidence: %+v", site)
				}
			}
			if !callers {
				if first != nil && !reflect.DeepEqual(first, result.Evidence) {
					t.Fatal("cache lost call-site evidence")
				}
				first = result.Evidence
			}
		}
		limited, err := engine.Trace(t.Context(), "Start", "End", "", false, 2)
		if err != nil || !limited.Limited || len(limited.Paths) != 0 {
			t.Fatalf("silent depth limit: %+v, %v", limited, err)
		}
		if _, err := engine.Trace(t.Context(), "Start", "Missing", "", false, 8); err == nil {
			t.Fatal("unknown destination silently accepted")
		}
	}
}

func TestTraceBoundsPathExplosionAndCancellation(t *testing.T) {
	root := t.TempDir()
	var source strings.Builder
	source.WriteString("package sample\nfunc Start() {\n")
	for i := range maxTracePaths + 1 {
		fmt.Fprintf(&source, " Branch%d()\n", i)
	}
	source.WriteString("}\n")
	for i := range maxTracePaths + 1 {
		fmt.Fprintf(&source, "func Branch%d() { End() }\n", i)
	}
	source.WriteString("func End() {}\n")
	writeFile(t, root, "wide.go", source.String())
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	result, err := e.Trace(t.Context(), "Start", "End", "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paths) != maxTracePaths || !result.Limited || result.Depth != maxTraceDepth || len(result.Evidence) > maxTraceEvidence {
		t.Fatalf("unbounded result: %+v", result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Trace(ctx, "Start", "End", "", false, 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestTraceWorkBudgetReportsIncompleteEvenWithoutPaths(t *testing.T) {
	// A layered DAG has exponentially many routes but only a few symbols.
	// The target is disconnected: a path limit alone cannot bound this query.
	g := &Graph{Nodes: []*Node{{ID: "start"}, {ID: "end"}}}
	previous := []string{"start"}
	for layer := range 20 {
		next := []string{fmt.Sprintf("a%d", layer), fmt.Sprintf("b%d", layer)}
		for _, id := range next {
			g.Nodes = append(g.Nodes, &Node{ID: id})
		}
		for _, from := range previous {
			for _, to := range next {
				g.Edges = append(g.Edges, &Edge{From: from, To: to, Kind: EdgeCalls})
			}
		}
		previous = next
	}
	g.build()
	budget := 100
	paths, limited := g.trace(t.Context(), "start", map[string]bool{"end": true}, false, 32, maxTracePaths, &budget)
	if len(paths) != 0 || !limited || budget < 0 {
		t.Fatalf("paths=%d limited=%t remaining=%d", len(paths), limited, budget)
	}
}

func TestTraceDoesNotPresentChangedSourceAsEvidenceForCachedEdges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "calls.go", "package sample; func Start() { End() }; func End() {}\n")
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	if _, err := e.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Within the normal freshness TTL the old graph may still be served.
	if err := os.WriteFile(filepath.Join(root, "calls.go"), []byte("package sample; func Start() {}; func End() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := e.Trace(t.Context(), "Start", "End", "", false, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SourceChanged || !result.EvidenceOmitted || len(result.Evidence) != 0 {
		t.Fatalf("misleading evidence: %+v", result)
	}
}
