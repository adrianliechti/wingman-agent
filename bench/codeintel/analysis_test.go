package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/graph"
	graphtool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/graph"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
)

func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, f := range fixtures() {
		if f.name == name {
			return f
		}
	}
	t.Fatal(name)
	return fixture{}
}

func TestGraphFindsBothCallPathsAfterLSPUnavailable(t *testing.T) {
	f := fixtureNamed(t, "multi_hop")
	root := writeFixture(t, f)
	service := language.New(root, filepath.Join(t.TempDir(), "graph.json"))
	defer service.Close()
	_, err := lsp.NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "outgoingCalls", "file_path": "entry.go", "symbol": "Handle"})
	if err == nil || !strings.Contains(err.Error(), "code_graph") {
		t.Fatalf("missing fallback: %v", err)
	}
	res, err := service.Graph().Trace(t.Context(), "Handle", "WriteRecord", "", false, 8)
	if err != nil {
		t.Fatal(err)
	}
	var paths [][]string
	for _, p := range res.Paths {
		var names []string
		for _, n := range p.Nodes {
			names = append(names, n.Name)
		}
		paths = append(paths, names)
	}
	slices.SortFunc(paths, func(a, b []string) int { return strings.Compare(strings.Join(a, "/"), strings.Join(b, "/")) })
	if !reflect.DeepEqual(paths, f.answer.(pathAnswer).Paths) {
		t.Fatalf("graph paths=%v", paths)
	}
	out, err := graphtool.NewTools(service.Graph())[0].Execute(t.Context(), map[string]any{"operation": "trace", "symbol": "Handle", "target": "WriteRecord"})
	if err != nil || !strings.Contains(out.Content, "2 path(s)") || strings.Contains(out.Content, "OtherEntry") {
		t.Fatalf("%s %v", out.Content, err)
	}
	if len(service.Manager().Activities()) != 0 {
		t.Fatal("structural fallback started a server")
	}
}

func TestGraphReceiverAmbiguityIsVisibleWithoutLSP(t *testing.T) {
	f := fixtureNamed(t, "receiver_references")
	root := writeFixture(t, f)
	engine := graph.New(root, filepath.Join(t.TempDir(), "graph.json"))
	out, err := graphtool.NewTools(engine)[0].Execute(t.Context(), map[string]any{"operation": "trace", "symbol": "Close", "direction": "callers"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"definitions share this name", "ambiguous name-based hop"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("graph concealed uncertainty %q: %s", want, out.Content)
		}
	}
}

func TestGraphContentPaginationRecoversAllSourceCandidates(t *testing.T) {
	f := fixtureNamed(t, "reference_completeness")
	root := writeFixture(t, f)
	engine := graph.New(root, filepath.Join(t.TempDir(), "graph.json"))
	inspect := graphtool.NewTools(engine)[0]
	seen := map[string]bool{}
	rawSeen := false
	for offset := 0; ; offset += 30 {
		res, err := engine.SearchContent(t.Context(), graph.ContentSearchOpts{Pattern: "Target", Limit: 30, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range res.Hits {
			if seen[hit.Node.ID] {
				t.Fatalf("duplicate %s", hit.Node.ID)
			}
			seen[hit.Node.ID] = true
		}
		if len(res.Raw) > 0 {
			rawSeen = true
		}
		out, err := inspect.Execute(t.Context(), map[string]any{"operation": "search_content", "pattern": "Target", "limit": 30, "offset": offset})
		if err != nil {
			t.Fatal(err)
		}
		if res.HasMore && !strings.Contains(out.Content, "More symbols available") {
			t.Fatal("silent truncation")
		}
		if !res.HasMore {
			break
		}
		if offset > 200 {
			t.Fatal("pagination did not terminate")
		}
	}
	for group := 0; group < 5; group++ {
		for i := 0; i < 23+group; i++ {
			name := fmtUse(group, i)
			matches, err := engine.Search(t.Context(), graph.SearchOpts{Query: name})
			if err != nil || len(matches) != 1 || !seen[matches[0].ID] {
				t.Fatalf("missed %s: %v", name, err)
			}
		}
	}
	if !rawSeen {
		t.Fatal("content search discarded raw comment/string candidates")
	}
}

func fmtUse(group, i int) string { return fmt.Sprintf("Use%d_%02d", group, i) }

func TestGraphRebuildAndReloadReflectSourceChanges(t *testing.T) {
	f := fixtureNamed(t, "multi_hop")
	root := writeFixture(t, f)
	cache := filepath.Join(t.TempDir(), "graph.json")
	engine := graph.New(root, cache)
	if _, err := engine.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Remove one complete branch and redirect another edge. Rebuild explicitly,
	// then load the persisted graph in a fresh engine; no timing-dependent sleep.
	if err := os.Remove(filepath.Join(root, "audit.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "entry.go"), []byte("package sample\nfunc Handle() { Process() }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "storage.go"), []byte("package sample\nfunc Persist() { Save() }\nfunc Save() { CheckPayload() }\nfunc WriteRecord() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []*graph.Engine{engine, graph.New(root, cache)} {
		if matches, err := candidate.Search(t.Context(), graph.SearchOpts{Query: "AppendAudit"}); err != nil || len(matches) != 0 {
			t.Fatalf("deleted symbol survived: %v %v", matches, err)
		}
		res, err := candidate.Trace(t.Context(), "Handle", "WriteRecord", "", false, 8)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Paths) != 0 {
			t.Fatalf("stale call edge survived: %v", res.Paths)
		}
	}
}

func TestGraphAnalyzesPythonAndTypeScriptWithoutLanguageServers(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"sample.py", "def leaf():\n    return 1\n\ndef middle():\n    return leaf()\n\ndef entry():\n    return middle()\n# entry calls leaf is only a comment\n"},
		{"sample.ts", "function leaf() { return 1; }\nfunction middle() { return leaf(); }\nfunction entry() { return middle(); }\n// entry calls leaf is only a comment\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.name), []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			engine := graph.New(root, filepath.Join(t.TempDir(), "graph.json"))
			out, err := graphtool.NewTools(engine)[0].Execute(t.Context(), map[string]any{"operation": "trace", "symbol": "entry", "target": "leaf"})
			if err != nil || !strings.Contains(out.Content, "entry → middle → leaf") {
				t.Fatalf("%s %v", out.Content, err)
			}
		})
	}
}
