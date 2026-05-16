package lsp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNewToolsExposesSingleLSPTool(t *testing.T) {
	tools := NewTools(nil)
	if len(tools) != 1 {
		t.Fatalf("len(NewTools) = %d, want 1", len(tools))
	}

	lspTool := tools[0]
	if lspTool.Name != "lsp" {
		t.Fatalf("tool name = %q, want lsp", lspTool.Name)
	}

	props := lspTool.Parameters["properties"].(map[string]any)
	operation := props["operation"].(map[string]any)
	enums := operation["enum"].([]string)

	for _, want := range []string{
		"diagnostics",
		"workspaceDiagnostics",
		"goToDefinition",
		"findReferences",
		"hover",
		"documentSymbol",
		"workspaceSymbol",
		"goToImplementation",
		"prepareCallHierarchy",
		"incomingCalls",
		"outgoingCalls",
	} {
		if !slices.Contains(enums, want) {
			t.Fatalf("operation enum missing %q: %#v", want, enums)
		}
	}

	required := lspTool.Parameters["required"].([]string)
	if !slices.Equal(required, []string{"operation"}) {
		t.Fatalf("required = %#v, want [operation]", required)
	}

	additional, ok := lspTool.Parameters["additionalProperties"].(bool)
	if !ok || additional {
		t.Fatalf("additionalProperties = %#v, want false", lspTool.Parameters["additionalProperties"])
	}
}

func TestParsePositionArgsUsesOneBasedInput(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "main.go")
	writeTestFile(t, testFile)

	path, line, column, err := parsePositionArgs(tmpDir, map[string]any{
		"path":   "main.go",
		"line":   12,
		"column": 5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != testFile {
		t.Fatalf("path = %q, want %q", path, testFile)
	}
	if line != 11 || column != 4 {
		t.Fatalf("line,column = %d,%d; want 11,4", line, column)
	}
}

func TestParsePositionArgsRejectsZeroBasedInput(t *testing.T) {
	tmpDir := t.TempDir()
	writeTestFile(t, filepath.Join(tmpDir, "main.go"))

	_, _, _, err := parsePositionArgs(tmpDir, map[string]any{
		"path":   "main.go",
		"line":   0,
		"column": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "line must be a positive 1-based integer") {
		t.Fatalf("expected line validation error, got: %v", err)
	}

	_, _, _, err = parsePositionArgs(tmpDir, map[string]any{
		"path":   "main.go",
		"line":   1,
		"column": 0,
	})
	if err == nil || !strings.Contains(err.Error(), "column must be a positive 1-based integer") {
		t.Fatalf("expected column validation error, got: %v", err)
	}
}

func TestParsePositionArgsRejectsFractionalInput(t *testing.T) {
	tmpDir := t.TempDir()
	writeTestFile(t, filepath.Join(tmpDir, "main.go"))

	_, _, _, err := parsePositionArgs(tmpDir, map[string]any{
		"path":   "main.go",
		"line":   1.5,
		"column": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "line must be a positive 1-based integer") {
		t.Fatalf("expected line validation error, got: %v", err)
	}
}

func TestResolveExistingFileRejectsDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	_, err := resolveExistingFile(tmpDir, ".")
	if err == nil || !strings.Contains(err.Error(), "path is a directory") {
		t.Fatalf("expected directory validation error, got: %v", err)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}
