package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/language"
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

func TestPaginationRetrievesEveryLocationInStableOrder(t *testing.T) {
	root := t.TempDir()
	uri, _ := lsp.ParseURI(fileuri.FromPath(filepath.Join(root, "missing.go")))
	var locations []lsp.Location
	for i := 224; i >= 0; i-- {
		locations = append(locations, lsp.Location{URI: uri, Range: lsp.Range{Start: lsp.Position{Line: uint32(i)}}})
	}
	var output strings.Builder
	for offset := 0; offset < 225; offset += 100 {
		part := formatLocations("References", locations, root, resultPage{offset: offset, limit: 100})
		output.WriteString(part)
		if offset < 200 && !strings.Contains(part, fmt.Sprintf("offset=%d", offset+100)) {
			t.Fatalf("missing continuation: %s", part)
		}
	}
	for i := 1; i <= 225; i++ {
		if count := strings.Count(output.String(), fmt.Sprintf("missing.go:%d:1\n", i)); count != 1 {
			t.Fatalf("line %d count=%d", i, count)
		}
	}
	if locations[0].Range.Start.Line != 224 {
		t.Fatal("pagination mutated server result")
	}
	if out := formatLocations("References", locations, root, resultPage{offset: 999, limit: 100}); strings.Contains(out, "missing.go:") || strings.Contains(out, "Continue with") {
		t.Fatal(out)
	}
}

func TestPageValidation(t *testing.T) {
	for _, args := range []map[string]any{{"offset": -1}, {"offset": 0.5}, {"limit": 0}, {"limit": 101}, {"limit": "10"}} {
		if _, err := parsePage(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestUnavailableLSPProvidesFallbackWithoutCleanResult(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	os.WriteFile(file, []byte("package main\nfunc run() {}\n"), 0600)
	service := language.New(root, filepath.Join(t.TempDir(), "graph.json"))
	defer service.Close()
	inspect := NewTools(service)[0]
	for _, operation := range []string{"status", "diagnostics", "workspaceSymbol", "goToDefinition", "goToTypeDefinition", "previewRename", "codeActions"} {
		t.Run(operation, func(t *testing.T) {
			result, err := inspect.Execute(t.Context(), map[string]any{"operation": operation, "file_path": "main.go", "symbol": "run", "new_name": "execute"})
			message := result.Content
			if err != nil {
				message += err.Error()
			}
			if !strings.Contains(message, "grep/glob/read") || strings.Contains(message, "No diagnostics found") || strings.Contains(message, "No symbols found") {
				t.Fatalf("unhelpful fallback: %s", message)
			}
		})
	}
	if len(service.Manager().Activities()) != 0 {
		t.Fatal("availability checks started a server")
	}
	// The structural path remains usable in a workspace with no managed tools.
	locations, err := service.DefinitionLocations(t.Context(), file, nil, 1, 5)
	if err != nil || len(locations) == 0 {
		t.Fatalf("structural fallback: %v %v", locations, err)
	}
}

func TestPreviewRejectsOutsideWorkspaceAndOverlappingEdits(t *testing.T) {
	root := t.TempDir()
	manager := lsp.NewManager(root)
	defer manager.Close()
	outside := positionTestFile(t, "abc\n")
	uri, _ := lsp.ParseURI(fileuri.FromPath(outside))
	edit := &lsp.WorkspaceEdit{Changes: map[lsp.DocumentURI][]lsp.TextEdit{uri: {{NewText: "x"}}}}
	if _, err := workspaceEditPreview(manager, edit); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("outside: %v", err)
	}
	path := filepath.Join(root, "file.txt")
	os.WriteFile(path, []byte("abc\n"), 0600)
	uri, _ = lsp.ParseURI(fileuri.FromPath(path))
	e := lsp.TextEdit{Range: lsp.Range{End: lsp.Position{Character: 2}}, NewText: "x"}
	edit.Changes = map[lsp.DocumentURI][]lsp.TextEdit{uri: {e, e}}
	if _, err := workspaceEditPreview(manager, edit); err == nil || !strings.Contains(err.Error(), "overlapping") {
		t.Fatalf("overlap: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "abc\n" {
		t.Fatal("preview changed a file")
	}
}

func TestPreviewProducesExactAtomicEditArguments(t *testing.T) {
	root := t.TempDir()
	manager := lsp.NewManager(root)
	defer manager.Close()
	path := filepath.Join(root, "file.go")
	before := "package example\n\nvar value = 1\n"
	os.WriteFile(path, []byte(before), 0600)
	uri, _ := lsp.ParseURI(fileuri.FromPath(path))
	edit := &lsp.WorkspaceEdit{Changes: map[lsp.DocumentURI][]lsp.TextEdit{uri: {{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 4}, End: lsp.Position{Line: 2, Character: 9}}, NewText: "count"}}}}
	preview, err := workspaceEditPreview(manager, edit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(preview.Content, "sha256") {
		t.Fatal("private snapshot leaked into the model-visible preview")
	}
	snapshots, err := tool.ResultFileSnapshots(preview.Metadata)
	if err != nil || len(snapshots) != 1 || snapshots[0].SHA256 == "" {
		t.Fatalf("missing private snapshot: %v %v", snapshots, err)
	}
	var payload struct {
		Edits []previewEdit `json:"edits"`
	}
	if err := json.Unmarshal([]byte(preview.Content[strings.Index(preview.Content, "{"):]), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Edits) != 1 || payload.Edits[0].Path != "file.go" {
		t.Fatalf("%+v", payload)
	}
	e := payload.Edits[0]
	after := strings.Replace(before, e.Old, e.New, 1)
	if after != "package example\n\nvar count = 1\n" {
		t.Fatal(after)
	}
	if data, _ := os.ReadFile(path); string(data) != before {
		t.Fatal("preview mutated source")
	}
}

func TestPreviewVersionsDistinguishDiskFilesFromOpenedDocuments(t *testing.T) {
	zero, one, two := int32(0), int32(1), int32(2)
	for _, tc := range []struct {
		requested *int32
		current   int
		opened    bool
		want      bool
	}{{nil, 0, false, true}, {&zero, 0, false, true}, {&one, 0, false, false}, {&zero, 1, true, false}, {&one, 1, true, true}, {&two, 1, true, false}} {
		if got := previewVersionMatches(tc.requested, tc.current, tc.opened); got != tc.want {
			t.Fatalf("requested=%v current=%d opened=%t: got %t", tc.requested, tc.current, tc.opened, got)
		}
	}
}

func TestTextEditsUTF16CRLFAndInvalidRanges(t *testing.T) {
	text := "😀value\r\nend\n"
	e := lsp.TextEdit{Range: lsp.Range{Start: lsp.Position{Character: 2}, End: lsp.Position{Character: 7}}, NewText: "count"}
	if out, err := applyTextEdits(text, []lsp.TextEdit{e}); err != nil || out != "😀count\r\nend\n" {
		t.Fatalf("%q %v", out, err)
	}
	for _, pos := range []lsp.Position{{Character: 1}, {Character: 8}, {Line: 10}} {
		if _, err := textOffset(text, pos); err == nil {
			t.Fatalf("accepted invalid position %v", pos)
		}
	}
	if offset, err := textOffset(text, lsp.Position{Line: 1, Character: 3}); err != nil || offset != len(text)-1 {
		t.Fatalf("line end offset=%d %v", offset, err)
	}
}

func FuzzCompactReplacement(f *testing.F) {
	for _, pair := range [][2]string{{"abc\n", "aXc\n"}, {"a\nb\n", "a\nnew\nb\n"}, {"x\nx\n", "x\n"}, {"a\na\na\n", "X\nY\na\n"}, {"😀x", "😀y"}, {"", "new"}, {"abc", ""}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, before, after string) {
		if before == after {
			return
		}
		old, next := compactReplacement(before, after)
		if before != "" && (old == "" || strings.Index(before, old) != strings.LastIndex(before, old)) {
			t.Fatalf("non-unique old_string %q in %q", old, before)
		}
		if got := strings.Replace(before, old, next, 1); got != after {
			t.Fatalf("replacement produced %q; want %q", got, after)
		}
	})
}
