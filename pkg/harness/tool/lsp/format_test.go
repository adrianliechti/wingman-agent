package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestFormatLocationsIncludesSourceText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri, err := lsp.ParseURI(fileuri.FromPath(path))
	if err != nil {
		t.Fatal(err)
	}
	locations := []lsp.Location{{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}}}
	result := formatLocations("References", locations, dir)
	if !strings.Contains(result, "References (1 found across 1 files):") || !strings.Contains(result, "main.go:3:6: func run() {}") {
		t.Fatalf("unexpected locations: %q", result)
	}
}

func TestFormatLocationsCapsOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri, err := lsp.ParseURI(fileuri.FromPath(path))
	if err != nil {
		t.Fatal(err)
	}
	locations := make([]lsp.Location, maxListedLocations+20)
	for i := range locations {
		locations[i] = lsp.Location{URI: uri}
	}
	result := formatLocations("References", locations, dir)
	note := fmt.Sprintf("showing first %d of %d locations", maxListedLocations, len(locations))
	if !strings.Contains(result, note) || strings.Count(result, "main.go:1:1") != maxListedLocations {
		t.Fatalf("unexpected truncated locations: %q", result)
	}
}

func TestFormatDefinitionsIncludesSnippet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	content := "package main\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	uri, err := lsp.ParseURI(fileuri.FromPath(path))
	if err != nil {
		t.Fatal(err)
	}
	locations := []lsp.Location{{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}}}
	result := formatDefinitions(locations, dir)
	if !strings.Contains(result, "main.go:3:6") || !strings.Contains(result, "func run() {") || !strings.Contains(result, "println(\"hi\")") {
		t.Fatalf("unexpected definitions: %q", result)
	}
}

func TestReferenceCountsAreCompleteDeduplicatedAndPageFiles(t *testing.T) {
	root := t.TempDir()
	var locations []lsp.Location
	for group := range 5 {
		uri, err := lsp.ParseURI(fileuri.FromPath(filepath.Join(root, fmt.Sprintf("calls_%d.go", group))))
		if err != nil {
			t.Fatal(err)
		}
		for line := range 23 + group {
			locations = append(locations, lsp.Location{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: uint32(line)}}})
		}
	}
	locations = append(locations, locations[0])
	full := formatReferenceCounts(locations, root, false, resultPage{limit: 100})
	for _, want := range []string{"125 across 5 files", "declaration excluded", "non-call references", "calls_0.go: 23", "calls_4.go: 27"} {
		if !strings.Contains(full, want) {
			t.Fatalf("missing %q: %s", want, full)
		}
	}
	page := formatReferenceCounts(locations, root, true, resultPage{offset: 2, limit: 2})
	if strings.Contains(page, "calls_0.go") || !strings.Contains(page, "calls_2.go: 25") || !strings.Contains(page, "offset=4") || !strings.Contains(page, "declaration included") {
		t.Fatal(page)
	}
	listed := formatLocations("References", locations[:125], root, resultPage{limit: 100}) + formatLocations("References", locations[:125], root, resultPage{offset: 100, limit: 100})
	if len(full)*5 >= len(listed) {
		t.Fatalf("count output not materially smaller: %d vs %d bytes", len(full), len(listed))
	}
	t.Logf("reference count output: %d bytes vs %d bytes of locations (source files absent)", len(full), len(listed))
}
