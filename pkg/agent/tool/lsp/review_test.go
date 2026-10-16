package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestInvalidArgumentsDoNotStartLSP(t *testing.T) {
	service, path, _ := fixtureService(t, "ok")
	inspect := NewTools(service)[0]
	for _, args := range []map[string]any{
		{"operation": "diagnostics", "file_path": true},
		{"operation": "workspaceSymbol", "query": 2},
		{"operation": "status", "file_path": nil},
		{"operation": "hover", "file_path": path, "symbol": "value", "column": 1},
		{"operation": "hover", "file_path": path, "symbol": "value", "line": nil},
		{"operation": "codeActions", "file_path": path, "action_title": 1},
		{"operation": "codeActions", "file_path": path, "line": 1},
		{"operation": "codeActions", "file_path": path, "line": 99, "column": 1},
		{"operation": "findReferences", "file_path": path, "line": 1, "column": 1, "output_mode": 1},
		{"operation": "findReferences", "file_path": path, "line": 1, "column": 1, "output_mode": "invalid"},
		{"operation": "findReferences", "file_path": path, "line": 1, "column": 1, "include_declaration": "yes"},
		{"operation": "status", "include_declaration": "yes"},
	} {
		if _, err := inspect.Execute(t.Context(), args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if len(service.Manager().Activities()) != 0 {
		t.Fatal("invalid arguments started a language server")
	}
	if len(NewTools(nil)) != 0 {
		t.Fatal("nil service exposed a tool")
	}
}

func TestCancelledLSPRequestsReturnContextError(t *testing.T) {
	service, path, _ := fixtureService(t, "ok")
	inspect := NewTools(service)[0]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := inspect.Execute(ctx, map[string]any{"operation": "status"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled status: %v", err)
	}
	// Start the fixture first, so the deadline falls in the diagnostics wait.
	if _, _, err := openFile(t.Context(), service.Manager(), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := inspect.Execute(ctx, map[string]any{"operation": "diagnostics", "file_path": path})
	if !errors.Is(err, context.DeadlineExceeded) || result.Content != "" {
		t.Fatalf("cancelled diagnostics returned success: %q %v", result.Content, err)
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
