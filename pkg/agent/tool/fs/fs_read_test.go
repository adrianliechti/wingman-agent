package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTool(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	content := "line1\nline2\nline3\nline4\nline5"
	testFile := filepath.Join(tmpDir, "test.txt")

	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	readTool := ReadTool(root)

	t.Run("read entire file", func(t *testing.T) {
		result, err := readTool.Execute(context.Background(), map[string]any{
			"path": "test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "line1") || !strings.Contains(result, "line5") {
			t.Errorf("expected full content, got: %s", result)
		}

		if !strings.Contains(result, "1\tline1") {
			t.Errorf("expected compact cat -n style line numbers, got: %s", result)
		}
	})

	t.Run("read with offset", func(t *testing.T) {
		result, err := readTool.Execute(context.Background(), map[string]any{
			"path":   "test.txt",
			"offset": float64(3),
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "line1") || strings.Contains(result, "line2") {
			t.Errorf("offset should skip first lines, got: %s", result)
		}

		if !strings.Contains(result, "line3") {
			t.Errorf("should contain line3, got: %s", result)
		}

		if !strings.Contains(result, "3\tline3") {
			t.Errorf("expected offset to start at 1-based line 3, got: %s", result)
		}
	})

	t.Run("read with limit", func(t *testing.T) {
		result, err := readTool.Execute(context.Background(), map[string]any{
			"path":  "test.txt",
			"limit": 2,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "line1") {
			t.Errorf("should contain line1, got: %s", result)
		}

		if strings.Contains(result, "line3") {
			t.Errorf("limit should cap returned lines, got: %s", result)
		}
		if !strings.Contains(result, "offset=3") {
			t.Errorf("expected continuation offset, got: %s", result)
		}
	})

	t.Run("read rejects non-positive limit", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path":  "test.txt",
			"limit": 0,
		})

		if err == nil || !strings.Contains(err.Error(), "limit must be") {
			t.Fatalf("expected limit validation error, got: %v", err)
		}
	})

	t.Run("read rejects fractional offset", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path":   "test.txt",
			"offset": 1.5,
		})

		if err == nil || !strings.Contains(err.Error(), "offset must be") {
			t.Fatalf("expected offset validation error, got: %v", err)
		}
	})

	t.Run("read rejects zero offset", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path":   "test.txt",
			"offset": 0,
		})

		if err == nil || !strings.Contains(err.Error(), "positive 1-based") {
			t.Fatalf("expected offset validation error, got: %v", err)
		}
	})

	t.Run("read offset past end returns reminder", func(t *testing.T) {
		result, err := readTool.Execute(context.Background(), map[string]any{
			"path":   "test.txt",
			"offset": 99,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "shorter than the provided offset (99)") {
			t.Errorf("expected offset reminder, got: %s", result)
		}
	})

	t.Run("read rejects directories", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path": ".",
		})

		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected directory error, got: %v", err)
		}
	})

	t.Run("read non-existent file", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path": "nonexistent.txt",
		})

		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("path outside workspace rejected", func(t *testing.T) {
		_, err := readTool.Execute(context.Background(), map[string]any{
			"path": "/etc/passwd",
		})

		if err == nil {
			t.Error("expected error for path outside workspace")
		}

		if !strings.Contains(err.Error(), "outside workspace") {
			t.Errorf("expected 'outside workspace' error, got: %v", err)
		}
	})

	t.Run("read with absolute path inside workspace", func(t *testing.T) {
		result, err := readTool.Execute(context.Background(), map[string]any{
			"path": testFile,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "line1") {
			t.Errorf("expected content, got: %s", result)
		}
	})

	t.Run("read rejects binary files", func(t *testing.T) {
		os.WriteFile(filepath.Join(tmpDir, "logo.png"), []byte("\x89PNG\r\n\x1a\n"), 0644)

		_, err := readTool.Execute(context.Background(), map[string]any{
			"path": "logo.png",
		})

		if err == nil {
			t.Fatal("expected error reading binary file, got nil")
		}

		if !strings.Contains(err.Error(), "binary") {
			t.Errorf("expected 'binary' in error, got: %v", err)
		}
	})

	t.Run("read svg as text", func(t *testing.T) {
		os.WriteFile(filepath.Join(tmpDir, "icon.svg"), []byte(`<svg><title>Logo</title></svg>`), 0644)

		result, err := readTool.Execute(context.Background(), map[string]any{
			"path": "icon.svg",
		})

		if err != nil {
			t.Fatalf("unexpected error reading svg: %v", err)
		}

		if !strings.Contains(result, "<title>Logo</title>") {
			t.Errorf("expected svg text, got: %s", result)
		}
	})
}
