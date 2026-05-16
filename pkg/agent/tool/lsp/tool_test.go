package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}
