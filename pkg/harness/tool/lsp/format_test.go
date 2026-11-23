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

func TestFormatLocationsListsEveryLocation(t *testing.T) {
	dir := t.TempDir()
	var locations []lsp.Location
	for _, file := range []string{"a.go", "b.go"} {
		uri, err := lsp.ParseURI(fileuri.FromPath(filepath.Join(dir, file)))
		if err != nil {
			t.Fatal(err)
		}
		for line := range 300 {
			locations = append(locations, lsp.Location{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: uint32(line)}}})
		}
	}
	result := formatLocations("References", locations, dir)
	if !strings.Contains(result, "References (600 found across 2 files):") || strings.Count(result, ".go:") != 600 {
		t.Fatalf("locations were left out: %d listed", strings.Count(result, ".go:"))
	}
	if got := formatLocations("References", nil, dir); got != "No references found" {
		t.Fatalf("empty result = %q", got)
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

func TestFormatSymbolsGroupsByFile(t *testing.T) {
	dir := t.TempDir()
	var symbols []lsp.SymbolInformation
	for index := range 250 {
		uri, err := lsp.ParseURI(fileuri.FromPath(filepath.Join(dir, fmt.Sprintf("f%d.go", index%2))))
		if err != nil {
			t.Fatal(err)
		}
		symbols = append(symbols, lsp.SymbolInformation{Location: lsp.Location{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: uint32(index)}}}})
		symbols[index].Name, symbols[index].Kind = fmt.Sprintf("Symbol%03d", index), lsp.SymbolKindFunction
	}
	result := formatSymbolInformation(symbols, dir)
	if !strings.Contains(result, "Symbols (250 found):") || strings.Count(result, "\nf0.go:\n") != 1 || strings.Count(result, "\nf1.go:\n") != 1 {
		t.Fatalf("symbols are not grouped once per file: %s", result)
	}
	if strings.Count(result, " (Function) - Line ") != 250 {
		t.Fatalf("symbols were left out: %s", result)
	}
	if got := formatSymbolInformation(nil, dir); got != "No symbols found" {
		t.Fatalf("empty result = %q", got)
	}
}
