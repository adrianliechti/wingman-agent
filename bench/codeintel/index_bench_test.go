package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/graph"
)

// A separate process with exactly one build makes process wall time and RSS
// comparable to one external CLI index command. It never changes source files
// or the shared index used by model trials.
func TestRepositoryColdIndexBenchmark(t *testing.T) {
	corpus, repo := os.Getenv("CODEINTEL_CORPUS"), os.Getenv("CODEINTEL_REPO")
	if corpus == "" || repo == "" {
		t.Skip("set CODEINTEL_CORPUS and CODEINTEL_REPO")
	}
	if !slices.Contains([]string{"gin", "django", "excalidraw", "vscode"}, repo) {
		t.Fatal("unknown repository")
	}
	engine := graph.New(filepath.Join(corpus, repo), filepath.Join(t.TempDir(), "graph.json"))
	start := time.Now()
	status, err := engine.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"repository": repo, "cold_index_ms": time.Since(start).Milliseconds(), "files": status.Files, "nodes": status.Nodes, "edges": status.Edges, "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0)}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, repo+".cold-index.json"), append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(encoded))
}

// One repository per process makes peak RSS and cold startup attributable.
// Only use the disposable corpus produced by prepare_repos.py. The edit probe
// restores original bytes and timestamps before returning.
func TestRepositoryIndexBenchmark(t *testing.T) {
	corpus, repo := os.Getenv("CODEINTEL_CORPUS"), os.Getenv("CODEINTEL_REPO")
	if corpus == "" || repo == "" {
		t.Skip("set CODEINTEL_CORPUS and CODEINTEL_REPO")
	}
	if !strings.Contains(" gin django excalidraw vscode ", " "+repo+" ") {
		t.Fatal("unknown repository")
	}
	root := filepath.Join(corpus, repo)
	cache := filepath.Join(corpus, repo+".graph.json")
	engine := graph.New(root, cache)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	status, err := engine.Index(t.Context())
	coldMS := time.Since(start).Milliseconds()
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	loaded := graph.New(root, cache)
	if !loaded.StatusOrLoad().Indexed {
		t.Fatal("snapshot failed to reload")
	}
	loadMS := time.Since(start).Milliseconds()
	skipped := map[string]int{}
	for _, issue := range status.Skipped {
		skipped[issue.Reason]++
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"repository": repo, "cold_index_ms": coldMS, "cache_load_ms": loadMS, "snapshot_bytes": info.Size(), "files": status.Files, "nodes": status.Nodes, "edges": status.Edges, "skipped": skipped, "edge_provenance": engine.EdgeStats(), "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "heap_in_use_bytes": after.HeapInuse, "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0)}
	// Use verify_repos.py before/after profiling to check source text hashes.
	// A no-change rebuild exposes our current whole-repository refresh cost
	// without mutating source or confounding model trials.
	start = time.Now()
	_, err = engine.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data["unchanged_reindex_ms"] = time.Since(start).Milliseconds()
	editFiles := map[string]string{"gin": "context.go", "django": "django/db/models/query.py", "excalidraw": "src/renderer/renderScene.ts", "vscode": "src/vs/workbench/services/extensions/common/rpcProtocol.ts"}
	editPath := filepath.Join(root, editFiles[repo])
	original, err := os.ReadFile(editPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.Stat(editPath)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := os.WriteFile(editPath, original, metadata.Mode().Perm()); err != nil {
			t.Error(err)
		}
		if err := os.Chtimes(editPath, metadata.ModTime(), metadata.ModTime()); err != nil {
			t.Error(err)
		}
	}
	defer restore()
	if err := os.WriteFile(editPath, append(slices.Clone(original), '\n'), metadata.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	_, err = engine.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data["one_file_edit_reindex_ms"] = time.Since(start).Milliseconds()
	restore()
	if _, err := engine.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, repo+".index.json"), append(bytes, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(bytes))
}
