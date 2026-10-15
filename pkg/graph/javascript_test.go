package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Excalidraw's scene entry points and throttleRAF use bound arrow functions.
// The grammar's inferred tags query only includes function declarations.
func TestJavaScriptBoundFunctions(t *testing.T) {
	for _, ext := range []string{"js", "ts", "tsx"} {
		t.Run(ext, func(t *testing.T) {
			root := t.TempDir()
			source := `function leaf() { return 1; }
export const render = () => leaf();
const schedule = function () { return render(); };
const ordinaryValue = 42;
function entry() { return schedule(); }
`
			writeFile(t, root, "scene."+ext, source)
			e := New(root, filepath.Join(t.TempDir(), "graph.json"))
			result, err := e.Trace(t.Context(), "entry", "leaf", "", false, 8)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Paths) != 1 {
				t.Fatalf("got %d paths, want entry -> schedule -> render -> leaf", len(result.Paths))
			}
			want := []string{"entry", "schedule", "render", "leaf"}
			if len(result.Paths[0].Nodes) != len(want) {
				t.Fatal(result.Paths)
			}
			for i, n := range result.Paths[0].Nodes {
				if n.Name != want[i] {
					t.Fatalf("step %d = %s, want %s", i, n.Name, want[i])
				}
			}
			found := map[string]int{}
			for _, symbol := range FileSymbols("scene."+ext, []byte(source)) {
				found[symbol.Name]++
			}
			for _, name := range want {
				if found[name] != 1 {
					t.Errorf("outline %s count=%d", name, found[name])
				}
			}
			if found["ordinaryValue"] != 0 {
				t.Fatal("non-callable constant misclassified as function")
			}
		})
	}
}

func TestTypeScriptGenericArrowFunction(t *testing.T) {
	for _, ext := range []string{"ts", "tsx"} {
		t.Run(ext, func(t *testing.T) {
			source := []byte("export const identity = <T,>(value: T): T => value;\n")
			syms := FileSymbols("generic."+ext, source)
			if len(syms) != 1 || syms[0].Name != "identity" || syms[0].Kind != KindFunction {
				t.Fatalf("generic function missing: %+v", syms)
			}
		})
	}
}

func TestJavaScriptNestedBoundFunctionOwnsItsCalls(t *testing.T) {
	for _, ext := range []string{"js", "ts", "tsx"} {
		t.Run(ext, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "nested."+ext, `function leaf() { return 1; }
function outer() {
  const nested = async () => leaf();
  return nested();
}
const values = function* () { yield leaf(); };
`)
			e := New(root, filepath.Join(t.TempDir(), "graph.json"))
			leaf, err := e.Neighborhood(t.Context(), "", "leaf", "")
			if err != nil {
				t.Fatal(err)
			}
			callers := map[string]bool{}
			for _, caller := range leaf.Callers {
				callers[caller.Name] = true
			}
			if len(callers) != 2 || !callers["nested"] || !callers["values"] {
				t.Fatalf("leaf callers = %v; want nested and values, without outer", callers)
			}
			outer, err := e.Neighborhood(t.Context(), "", "outer", "")
			if err != nil || len(outer.Callees) != 1 || outer.Callees[0].Name != "nested" {
				t.Fatalf("outer must call nested directly: %+v, %v", outer, err)
			}
		})
	}
}

func TestJavaScriptRebuildsSnapshotWithoutBoundFunctions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "scene.js", "const entry = () => leaf();\nconst leaf = () => 1;\n")
	cache := filepath.Join(t.TempDir(), "graph.json")
	if _, err := New(root, cache).Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var old snapshot
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	// Version 10 could have current file metadata but no bound functions.
	old.Version, old.Nodes, old.Edges = 10, nil, nil
	data, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := New(root, cache).Trace(t.Context(), "entry", "leaf", "", false, 8)
	if err != nil || len(result.Paths) != 1 || len(result.Paths[0].Nodes) != 2 {
		t.Fatalf("old cache must be rebuilt: %+v, %v", result, err)
	}
}
