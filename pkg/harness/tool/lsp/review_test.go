package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestAmbiguousTextRequiresCoordinates(t *testing.T) {
	path := positionTestFile(t, "func run() { run() }\n")
	if _, err := positionOfSymbolOnLine(path, 1, "run"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("line ambiguity: %v", err)
	}
	if _, err := positionOfSymbol(path, "run"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("file ambiguity: %v", err)
	}
	path = positionTestFile(t, "// 😀\nfunc unique() {}\n")
	if pos, err := positionOfSymbol(path, "unique"); err != nil || pos.Line != 1 || pos.Character != 5 {
		t.Fatalf("unique: %v %v", pos, err)
	}
}

func TestWorkspaceContainmentHandlesRootAndCaseSensitiveSiblings(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if !pathInsideWorkspace(filepath.Join(root, "project", "a.go"), root) {
		t.Fatal("filesystem root rejected a child")
	}
	base := t.TempDir()
	workspace := filepath.Join(base, "Repo")
	sibling := filepath.Join(base, "repo", "a.go")
	rel, err := filepath.Rel(workspace, sibling)
	if err == nil && !filepath.IsLocal(rel) && pathInsideWorkspace(sibling, workspace) {
		t.Fatal("case-sensitive sibling accepted as workspace content")
	}
}

func TestNonFileLocationsPreserveURI(t *testing.T) {
	const uri = "jdt://contents/example.jar/Example.class"
	parsed, err := lsp.ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	location := lsp.Location{URI: parsed}
	for name, output := range map[string]string{
		"references": formatLocations("References", []lsp.Location{location}, t.TempDir()),
		"definition": formatDefinitions([]lsp.Location{location}, t.TempDir()),
	} {
		if !strings.Contains(output, uri+":1:1") {
			t.Errorf("%s lost URI: %s", name, output)
		}
	}
}

func TestLocationContextSkipsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.go")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (2<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if lines := (sourceCache{}).lines(path); lines != nil {
		t.Fatal("read oversized optional source context")
	}
}
