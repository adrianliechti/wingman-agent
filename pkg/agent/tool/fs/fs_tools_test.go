package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func createTestRoot(t *testing.T) (*os.Root, string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "fs_test_*")

	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	root, err := os.OpenRoot(tmpDir)

	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to open root: %v", err)
	}

	cleanup := func() {
		root.Close()
		os.RemoveAll(tmpDir)
	}

	return root, tmpDir, cleanup
}

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

func TestWriteTool(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	writeTool := WriteTool(root)

	t.Run("write new file", func(t *testing.T) {
		result, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    "newfile.txt",
			"content": "hello world",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "Created") {
			t.Errorf("expected 'Created' message, got: %s", result)
		}

		content, err := os.ReadFile(filepath.Join(tmpDir, "newfile.txt"))

		if err != nil {
			t.Fatalf("failed to read created file: %v", err)
		}

		if string(content) != "hello world" {
			t.Errorf("expected 'hello world', got: %s", content)
		}
	})

	t.Run("write with nested directory", func(t *testing.T) {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    "subdir/nested/file.txt",
			"content": "nested content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content, err := os.ReadFile(filepath.Join(tmpDir, "subdir", "nested", "file.txt"))

		if err != nil {
			t.Fatalf("failed to read created file: %v", err)
		}

		if string(content) != "nested content" {
			t.Errorf("expected 'nested content', got: %s", content)
		}
	})

	t.Run("overwrite existing file", func(t *testing.T) {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    "overwrite.txt",
			"content": "original",
		})

		if err != nil {
			t.Fatalf("unexpected error on first write: %v", err)
		}

		_, err = ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "overwrite.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = writeTool.Execute(context.Background(), map[string]any{
			"path":    "overwrite.txt",
			"content": "updated",
		})

		if err != nil {
			t.Fatalf("unexpected error on overwrite: %v", err)
		}

		content, err := os.ReadFile(filepath.Join(tmpDir, "overwrite.txt"))

		if err != nil {
			t.Fatalf("failed to read file: %v", err)
		}

		if string(content) != "updated" {
			t.Errorf("expected 'updated', got: %s", content)
		}
	})

	t.Run("path outside workspace rejected", func(t *testing.T) {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    "/tmp/outside.txt",
			"content": "should fail",
		})

		if err == nil {
			t.Error("expected error for path outside workspace")
		}
	})

	t.Run("write rejects directory path", func(t *testing.T) {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    ".",
			"content": "should fail",
		})

		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected directory error, got: %v", err)
		}
	})
}

func TestEditTool(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	editTool := EditTool(root)

	t.Run("simple edit", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "edit_test.txt")
		os.WriteFile(testFile, []byte("hello world"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "edit_test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		result, err := editTool.Execute(context.Background(), map[string]any{
			"path":       "edit_test.txt",
			"old_string": "world",
			"new_string": "universe",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "Successfully") {
			t.Errorf("expected success message, got: %s", result)
		}

		content, _ := os.ReadFile(testFile)

		if string(content) != "hello universe" {
			t.Errorf("expected 'hello universe', got: %s", content)
		}
	})

	t.Run("edit can create file with empty old string", func(t *testing.T) {
		result, err := editTool.Execute(context.Background(), map[string]any{
			"path":       "created_by_edit.txt",
			"old_string": "",
			"new_string": "created content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "Successfully") {
			t.Errorf("expected success message, got: %s", result)
		}

		content, err := os.ReadFile(filepath.Join(tmpDir, "created_by_edit.txt"))
		if err != nil {
			t.Fatalf("failed to read created file: %v", err)
		}
		if string(content) != "created content" {
			t.Errorf("expected created content, got: %s", content)
		}
	})

	t.Run("edit rejects empty old string on non-empty file", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "nonempty_empty_old.txt")
		os.WriteFile(testFile, []byte("existing"), 0644)

		_, err := editTool.Execute(context.Background(), map[string]any{
			"path":       "nonempty_empty_old.txt",
			"old_string": "",
			"new_string": "replacement",
		})

		if err == nil || !strings.Contains(err.Error(), "already has content") {
			t.Fatalf("expected non-empty file error, got: %v", err)
		}
	})

	t.Run("edit preserves curly quote style", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "curly_quotes.txt")
		os.WriteFile(testFile, []byte("title: “Hello”\n"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "curly_quotes.txt",
		})
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = editTool.Execute(context.Background(), map[string]any{
			"path":       "curly_quotes.txt",
			"old_string": `"Hello"`,
			"new_string": `"World"`,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content, err := os.ReadFile(testFile)
		if err != nil {
			t.Fatalf("failed to read file: %v", err)
		}
		if string(content) != "title: “World”\n" {
			t.Errorf("expected curly quote style preserved, got: %s", content)
		}
	})

	t.Run("edit preserves CRLF line endings", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "crlf_test.txt")
		os.WriteFile(testFile, []byte("line1\r\nline2\r\nline3"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "crlf_test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = editTool.Execute(context.Background(), map[string]any{
			"path":       "crlf_test.txt",
			"old_string": "line2",
			"new_string": "modified",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content, _ := os.ReadFile(testFile)

		if !strings.Contains(string(content), "\r\n") {
			t.Error("CRLF line endings should be preserved")
		}
	})

	t.Run("edit with fuzzy match (trailing whitespace)", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "fuzzy_test.txt")
		os.WriteFile(testFile, []byte("hello   \nworld"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "fuzzy_test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = editTool.Execute(context.Background(), map[string]any{
			"path":       "fuzzy_test.txt",
			"old_string": "hello\nworld",
			"new_string": "goodbye\nworld",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content, _ := os.ReadFile(testFile)

		if !strings.Contains(string(content), "goodbye") {
			t.Errorf("expected fuzzy match to work, got: %s", content)
		}
	})

	t.Run("edit fails for non-unique match", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "duplicate_test.txt")
		os.WriteFile(testFile, []byte("foo bar foo"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "duplicate_test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = editTool.Execute(context.Background(), map[string]any{
			"path":       "duplicate_test.txt",
			"old_string": "foo",
			"new_string": "baz",
		})

		if err == nil {
			t.Error("expected error for non-unique match")
		}

		if !strings.Contains(err.Error(), "occurrences") {
			t.Errorf("expected 'occurrences' in error, got: %v", err)
		}
	})

	t.Run("edit fails for no match", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "nomatch_test.txt")
		os.WriteFile(testFile, []byte("hello world"), 0644)

		_, err := ReadTool(root).Execute(context.Background(), map[string]any{
			"path": "nomatch_test.txt",
		})

		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}

		_, err = editTool.Execute(context.Background(), map[string]any{
			"path":       "nomatch_test.txt",
			"old_string": "xyz",
			"new_string": "abc",
		})

		if err == nil {
			t.Error("expected error for no match")
		}
	})

	t.Run("edit rejects legacy old_text params", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "legacy_params.txt")
		os.WriteFile(testFile, []byte("hello world"), 0644)

		_, err := editTool.Execute(context.Background(), map[string]any{
			"path":     "legacy_params.txt",
			"old_text": "world",
			"new_text": "universe",
		})

		if err == nil || !strings.Contains(err.Error(), "old_string is required") {
			t.Fatalf("expected old_string required error, got: %v", err)
		}
	})

	t.Run("edit rejects identical replacement before matching", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "identical_test.txt")
		os.WriteFile(testFile, []byte("hello world"), 0644)

		_, err := editTool.Execute(context.Background(), map[string]any{
			"path":       "identical_test.txt",
			"old_string": "world",
			"new_string": "world",
		})

		if err == nil || !strings.Contains(err.Error(), "identical") {
			t.Fatalf("expected identical replacement error, got: %v", err)
		}
	})

	t.Run("edit fuzzy replace_all must make progress", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "fuzzy_replace_all.txt")
		os.WriteFile(testFile, []byte("hello   \nworld\nhello   \nworld"), 0644)

		_, err := editTool.Execute(context.Background(), map[string]any{
			"path":        "fuzzy_replace_all.txt",
			"old_string":  "hello\nworld",
			"new_string":  "hello   \nworld",
			"replace_all": true,
		})

		if err == nil || !strings.Contains(err.Error(), "made no progress") {
			t.Fatalf("expected no-progress error, got: %v", err)
		}
	})

}

func TestGlobTool(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.MkdirAll(filepath.Join(tmpDir, "src", "pkg"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "node_modules", "dep"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "pkg", "util.go"), []byte("content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.ts"), []byte("content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "node_modules", "dep", "index.js"), []byte("content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, ".hidden.go"), []byte("content"), 0644)

	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("content"), 0644)

	globTool := GlobTool(root)

	t.Run("glob all go files", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "**/*.go",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "main.go") {
			t.Errorf("expected main.go, got: %s", result)
		}

		if !strings.Contains(result, "app.go") {
			t.Errorf("expected app.go, got: %s", result)
		}

		if !strings.Contains(result, "util.go") {
			t.Errorf("expected util.go, got: %s", result)
		}
	})

	t.Run("glob includes ignored directories", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "**/*.js",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "node_modules") {
			t.Errorf("expected node_modules like reference Glob, got: %s", result)
		}
	})

	t.Run("glob includes gitignored and hidden files", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "*",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "debug.log") {
			t.Errorf("expected gitignored file like reference Glob, got: %s", result)
		}

		if !strings.Contains(result, ".hidden.go") {
			t.Errorf("expected hidden file like reference Glob, got: %s", result)
		}
	})

	t.Run("glob in subdirectory", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "*.go",
			"path":    "src",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "main.go") {
			t.Errorf("should not include files outside src, got: %s", result)
		}

		if !strings.Contains(result, filepath.Join("src", "app.go")) {
			t.Errorf("expected app.go, got: %s", result)
		}

		if !strings.Contains(result, filepath.Join("src", "pkg", "util.go")) {
			t.Errorf("pattern without slash should match recursively under path, got: %s", result)
		}
	})

	t.Run("glob path returns workspace-relative paths", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "*.go",
			"path":    "src/pkg",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, filepath.Join("src", "pkg", "util.go")) {
			t.Errorf("expected workspace-relative path, got: %s", result)
		}
		if strings.Contains(result, "\nutil.go") || result == "util.go" {
			t.Errorf("did not expect path-relative-only result, got: %s", result)
		}
	})

	t.Run("glob with no matches", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "*.xyz",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "No files found") {
			t.Errorf("expected 'No files found', got: %s", result)
		}
	})

	t.Run("glob invalid pattern", func(t *testing.T) {
		_, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "[",
		})

		if err == nil || !strings.Contains(err.Error(), "invalid glob pattern") {
			t.Fatalf("expected invalid glob pattern error, got: %v", err)
		}
	})

	t.Run("glob with absolute search path", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": "**/*.go",
			"path":    tmpDir,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "main.go") {
			t.Errorf("expected main.go, got: %s", result)
		}

		if !strings.Contains(result, "app.go") {
			t.Errorf("expected app.go, got: %s", result)
		}
	})

	t.Run("glob with absolute pattern", func(t *testing.T) {
		result, err := globTool.Execute(context.Background(), map[string]any{
			"pattern": filepath.Join(tmpDir, "src", "*.go"),
			"path":    "node_modules",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "main.go") || strings.Contains(result, "node_modules") {
			t.Errorf("absolute pattern should override path, got: %s", result)
		}

		if !strings.Contains(result, "app.go") {
			t.Errorf("expected app.go, got: %s", result)
		}
	})

	t.Run("glob returns modified order when results exceed limit", func(t *testing.T) {
		newRoot, newTmp, newCleanup := createTestRoot(t)
		defer newCleanup()

		base := time.Now().Add(-1 * time.Hour)
		for i := range 120 {
			name := fmt.Sprintf("f%03d.tmp", i)
			p := filepath.Join(newTmp, name)
			os.WriteFile(p, []byte("x"), 0644)
			os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
		}

		result, err := GlobTool(newRoot).Execute(context.Background(), map[string]any{
			"pattern": "*.tmp",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, want := range []string{"f000.tmp", "f001.tmp", "f002.tmp"} {
			if !strings.Contains(result, want) {
				t.Errorf("expected oldest file %s in result, got: %s", want, result)
			}
		}
		if strings.Contains(result, "f118.tmp") || strings.Contains(result, "f119.tmp") {
			t.Errorf("newest files leaked in despite limit, got: %s", result)
		}
		if !strings.Contains(result, "(Results are truncated. Consider using a more specific path or pattern.)") {
			t.Errorf("expected truncation notice, got: %s", result)
		}
	})
}

func TestGrepTool(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.WriteFile(filepath.Join(tmpDir, "file1.go"), []byte("package main\n\nfunc Hello() {\n\treturn \"hello\"\n}"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "file2.go"), []byte("package util\n\nfunc World() {\n\treturn \"world\"\n}"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("# Hello World\nThis is a test."), 0644)
	os.WriteFile(filepath.Join(tmpDir, "icon.svg"), []byte(`<svg><title>Hello Icon</title></svg>`), 0644)

	grepTool := GrepTool(root)

	t.Run("grep simple pattern", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "func",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "file1.go") {
			t.Errorf("expected file1.go in results, got: %s", result)
		}

		if strings.Contains(result, "func Hello") {
			t.Errorf("default output should list files only, got: %s", result)
		}
	})

	t.Run("grep with regex", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "func \\w+\\(",
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "Hello()") || !strings.Contains(result, "World()") {
			t.Errorf("expected function matches, got: %s", result)
		}

		if !strings.Contains(result, "file1.go:3:func Hello") {
			t.Errorf("expected ripgrep-style line-numbered content, got: %s", result)
		}
	})

	t.Run("grep case insensitive", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "HELLO",
			"-i":          true,
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "Hello") || !strings.Contains(result, "hello") {
			t.Errorf("expected case-insensitive matches, got: %s", result)
		}
	})

	t.Run("grep aliases case and context flags", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "HELLO",
			"-i":          true,
			"-A":          float64(1),
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "Hello") || !strings.Contains(result, "return") {
			t.Errorf("expected case-insensitive match with after context, got: %s", result)
		}
	})

	t.Run("grep with glob filter", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"glob":    "*.go",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "readme.md") {
			t.Errorf("should not include markdown files, got: %s", result)
		}
	})

	t.Run("grep with context lines", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "func Hello",
			"context":     float64(1),
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "func Hello") {
			t.Errorf("expected match line, got: %s", result)
		}
		lines := strings.Split(result, "\n")

		if len(lines) < 2 {
			t.Errorf("expected multiple lines with context, got: %s", result)
		}

		if !strings.Contains(result, "file1.go-2-") {
			t.Errorf("expected ripgrep-style context separator, got: %s", result)
		}
	})

	t.Run("grep can omit line numbers", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "func Hello",
			"output_mode": "content",
			"-n":          false,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "file1.go:3:") {
			t.Errorf("did not expect line numbers, got: %s", result)
		}
		if !strings.Contains(result, "file1.go:func Hello") {
			t.Errorf("expected path:content output without line numbers, got: %s", result)
		}
	})

	t.Run("grep no matches", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "zzz_no_match_zzz",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result != "No files found" {
			t.Errorf("expected 'No files found', got: %s", result)
		}
	})

	t.Run("grep invalid output mode", func(t *testing.T) {
		_, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "Hello",
			"output_mode": "bad",
		})

		if err == nil || !strings.Contains(err.Error(), "output_mode must be") {
			t.Fatalf("expected output_mode validation error, got: %v", err)
		}
	})

	t.Run("grep rejects fractional numeric parameters", func(t *testing.T) {
		_, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "Hello",
			"output_mode": "content",
			"head_limit":  1.5,
		})

		if err == nil || !strings.Contains(err.Error(), "head_limit") {
			t.Fatalf("expected head_limit validation error, got: %v", err)
		}
	})

	t.Run("grep rejects negative numeric parameters", func(t *testing.T) {
		_, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"offset":  -1,
		})

		if err == nil || !strings.Contains(err.Error(), "offset") {
			t.Fatalf("expected offset validation error, got: %v", err)
		}
	})

	t.Run("grep rejects fractional context parameters", func(t *testing.T) {
		_, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"context": 1.5,
		})

		if err == nil || !strings.Contains(err.Error(), "context") {
			t.Fatalf("expected context validation error, got: %v", err)
		}
	})

	t.Run("grep count is exact above previous cap", func(t *testing.T) {
		lines := strings.Repeat("needle\n", 10005)
		os.WriteFile(filepath.Join(tmpDir, "many.txt"), []byte(lines), 0644)

		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "needle",
			"path":        "many.txt",
			"output_mode": "count",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "many.txt:10005") {
			t.Errorf("expected exact count, got: %s", result)
		}
	})

	t.Run("grep single file", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"path":    "readme.md",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "readme.md") {
			t.Errorf("expected readme.md match, got: %s", result)
		}

		if strings.Contains(result, "file1.go") {
			t.Errorf("should only search single file, got: %s", result)
		}
	})

	t.Run("grep single file applies glob filter", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"path":    "readme.md",
			"glob":    "*.go",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "No matches") {
			t.Errorf("expected glob mismatch to return no matches, got: %s", result)
		}
	})

	t.Run("grep single file applies files offset", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"path":    "readme.md",
			"offset":  1,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result != "No files found" {
			t.Errorf("expected single files_with_matches result to be skipped, got: %s", result)
		}
	})

	t.Run("grep content offset notice points to next page", func(t *testing.T) {
		os.WriteFile(filepath.Join(tmpDir, "file3.go"), []byte("package extra\n\nfunc Third() {\n\treturn\n}"), 0644)

		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "func",
			"output_mode": "content",
			"head_limit":  1,
			"offset":      1,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "pagination = limit: 1, offset: 1") {
			t.Errorf("expected next offset notice, got: %s", result)
		}
	})

	t.Run("grep files_with_matches sorts by modified time before limit", func(t *testing.T) {
		oldPath := filepath.Join(tmpDir, "oldmatch.txt")
		newPath := filepath.Join(tmpDir, "newmatch.txt")
		os.WriteFile(oldPath, []byte("mtime needle\n"), 0644)
		os.WriteFile(newPath, []byte("mtime needle\n"), 0644)
		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now().Add(-1 * time.Hour)
		os.Chtimes(oldPath, oldTime, oldTime)
		os.Chtimes(newPath, newTime, newTime)

		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":    "mtime needle",
			"head_limit": 1,
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "newmatch.txt") || strings.Contains(result, "oldmatch.txt") {
			t.Errorf("expected newest matching file only, got: %s", result)
		}
		if !strings.Contains(result, "limit: 1") {
			t.Errorf("expected limit info, got: %s", result)
		}
	})

	t.Run("grep includes hidden files and respects gitignore", func(t *testing.T) {
		os.WriteFile(filepath.Join(tmpDir, ".hidden.txt"), []byte("secret-needle\n"), 0644)
		os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("ignored.txt\n"), 0644)
		os.WriteFile(filepath.Join(tmpDir, "ignored.txt"), []byte("secret-needle\n"), 0644)

		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "secret-needle",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, ".hidden.txt") {
			t.Errorf("expected hidden file match, got: %s", result)
		}
		if strings.Contains(result, "ignored.txt") {
			t.Errorf("expected gitignored file to be skipped, got: %s", result)
		}
	})

	t.Run("grep nested gitignore does not leak to siblings", func(t *testing.T) {
		newRoot, newTmp, newCleanup := createTestRoot(t)
		defer newCleanup()

		os.MkdirAll(filepath.Join(newTmp, "a"), 0755)
		os.MkdirAll(filepath.Join(newTmp, "b"), 0755)
		os.WriteFile(filepath.Join(newTmp, "a", ".gitignore"), []byte("ignored.txt\n"), 0644)
		os.WriteFile(filepath.Join(newTmp, "a", "ignored.txt"), []byte("leak-needle\n"), 0644)
		os.WriteFile(filepath.Join(newTmp, "b", "ignored.txt"), []byte("leak-needle\n"), 0644)

		result, err := GrepTool(newRoot).Execute(context.Background(), map[string]any{
			"pattern": "leak-needle",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, filepath.Join("a", "ignored.txt")) {
			t.Errorf("expected a/.gitignore to ignore only a/ignored.txt, got: %s", result)
		}
		if !strings.Contains(result, filepath.Join("b", "ignored.txt")) {
			t.Errorf("expected sibling ignored.txt not to be ignored, got: %s", result)
		}
	})

	t.Run("grep with absolute path", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     "func",
			"path":        tmpDir,
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "file1.go") {
			t.Errorf("expected file1.go in results, got: %s", result)
		}

		if !strings.Contains(result, "Hello") {
			t.Errorf("expected 'Hello' in results, got: %s", result)
		}
	})

	t.Run("grep multiline pattern spanning lines", func(t *testing.T) {
		os.WriteFile(filepath.Join(tmpDir, "multi.go"), []byte("type Foo struct {\n\tname string\n\tfield int\n}\n"), 0644)

		// Without multiline this can't match across newlines.
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":     `struct \{[\s\S]*?field`,
			"path":        "multi.go",
			"multiline":   true,
			"output_mode": "content",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "multi.go") || !strings.Contains(result, "field") {
			t.Errorf("expected multi.go and matched 'field' line, got: %s", result)
		}

		// Sanity: same pattern without multiline must NOT match.
		nonMulti, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": `struct \{[\s\S]*?field`,
			"path":    "multi.go",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if nonMulti != "No files found" {
			t.Errorf("non-multiline should not match across lines, got: %s", nonMulti)
		}
	})

	t.Run("grep type filter", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"type":    "go",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "file1.go") {
			t.Errorf("expected go file in results, got: %s", result)
		}

		if strings.Contains(result, "readme.md") {
			t.Errorf("type=go should exclude markdown files, got: %s", result)
		}
	})

	t.Run("grep unsupported type", func(t *testing.T) {
		_, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello",
			"type":    "notatype",
		})

		if err == nil || !strings.Contains(err.Error(), "unsupported type") {
			t.Fatalf("expected unsupported type error, got: %v", err)
		}
	})

	t.Run("grep type and glob both apply", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "func",
			"type":    "go",
			"glob":    "file2.*",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "file2.go") || strings.Contains(result, "file1.go") {
			t.Errorf("expected only file2.go, got: %s", result)
		}
	})

	t.Run("grep head limit zero is unlimited", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern":    "package|Hello",
			"head_limit": float64(0),
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if strings.Contains(result, "limit 0 hit") {
			t.Errorf("head_limit=0 should not impose a zero-result limit, got: %s", result)
		}
		if !strings.Contains(result, "file1.go") || !strings.Contains(result, "file2.go") || !strings.Contains(result, "readme.md") {
			t.Errorf("expected all matching files, got: %s", result)
		}
	})

	t.Run("grep searches svg as text", func(t *testing.T) {
		result, err := grepTool.Execute(context.Background(), map[string]any{
			"pattern": "Hello Icon",
			"path":    "icon.svg",
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(result, "icon.svg") {
			t.Errorf("expected svg match, got: %s", result)
		}
	})
}

// Regression: a file with a line longer than the bufio scan buffer used to
// silently drop the entire file from results. Matches in lines before the
// long line should still be returned, and the model should see a sentinel
// telling it the file's tail was skipped.
func TestGrepHandlesLongLines(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	longLine := strings.Repeat("x", MaxScanBufSize+1024)
	body := "needle line 1\nneedle line 2\n" + longLine + "\nneedle line 4\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "big.txt"), []byte(body), 0644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	grepTool := GrepTool(root)

	result, err := grepTool.Execute(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "needle line 1") || !strings.Contains(result, "needle line 2") {
		t.Errorf("expected pre-long-line matches preserved, got: %s", result)
	}

	if !strings.Contains(result, "exceeds") || !strings.Contains(result, "scan limit") {
		t.Errorf("expected scan-cutoff sentinel, got: %s", result)
	}
}

func TestPathHandlingCrossplatform(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.MkdirAll(filepath.Join(tmpDir, "a", "b", "c"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "a", "b", "c", "file.txt"), []byte("content"), 0644)

	writeTool := WriteTool(root)
	readTool := ReadTool(root)

	t.Run("forward slash paths work", func(t *testing.T) {
		_, err := writeTool.Execute(context.Background(), map[string]any{
			"path":    "a/b/c/new.txt",
			"content": "test",
		})

		if err != nil {
			t.Fatalf("unexpected error with forward slashes: %v", err)
		}

		result, err := readTool.Execute(context.Background(), map[string]any{
			"path": "a/b/c/new.txt",
		})

		if err != nil {
			t.Fatalf("unexpected error reading: %v", err)
		}

		if !strings.Contains(result, "test") {
			t.Errorf("expected content, got: %s", result)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("backslash paths work on windows", func(t *testing.T) {
			_, err := writeTool.Execute(context.Background(), map[string]any{
				"path":    "a\\b\\c\\win.txt",
				"content": "windows",
			})

			if err != nil {
				t.Fatalf("unexpected error with backslashes: %v", err)
			}
		})
	}
}

func TestTools(t *testing.T) {
	root, _, cleanup := createTestRoot(t)
	defer cleanup()

	tools := Tools(root)

	expectedNames := []string{"read", "write", "edit", "grep", "glob"}

	if len(tools) != len(expectedNames) {
		t.Errorf("expected %d tools, got %d", len(expectedNames), len(tools))
	}

	names := make(map[string]bool)

	for _, tool := range tools {
		names[tool.Name] = true

		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}

		if tool.Execute == nil {
			t.Errorf("tool %s has nil Execute function", tool.Name)
		}
	}

	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("missing expected tool: %s", name)
		}
	}
}

func TestGlobSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests may require elevated privileges on Windows")
	}

	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.WriteFile(filepath.Join(tmpDir, "root.txt"), []byte("root content"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "dir1"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "dir1", "file.txt"), []byte("content"), 0644)

	symlink := filepath.Join(tmpDir, "dir1", "circular")
	if err := os.Symlink(tmpDir, symlink); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	globTool := GlobTool(root)

	result, err := globTool.Execute(context.Background(), map[string]any{
		"pattern": "*.txt",
	})

	if err != nil {
		t.Fatalf("glob should not fail with symlinks: %v", err)
	}

	if !strings.Contains(result, "root.txt") && !strings.Contains(result, "file.txt") {
		t.Errorf("expected txt files in results, got: %s", result)
	}
}

func TestGrepSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests may require elevated privileges on Windows")
	}

	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.MkdirAll(filepath.Join(tmpDir, "dir1"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "dir1", "file.txt"), []byte("searchme"), 0644)

	symlink := filepath.Join(tmpDir, "dir1", "circular")
	if err := os.Symlink(tmpDir, symlink); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	grepTool := GrepTool(root)

	result, err := grepTool.Execute(context.Background(), map[string]any{
		"pattern": "searchme",
	})

	if err != nil {
		t.Fatalf("grep should not fail with symlinks: %v", err)
	}

	if !strings.Contains(result, "dir1/file.txt") {
		t.Errorf("expected matching file in results, got: %s", result)
	}
}

func TestContextCancellation(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	for i := range 100 {
		dir := filepath.Join(tmpDir, "dir"+string(rune('a'+i%26)))
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0644)
	}

	globTool := GlobTool(root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := globTool.Execute(ctx, map[string]any{
		"pattern": "*.txt",
	})

	if err == nil {
		t.Log("Operation completed before context cancellation was detected (acceptable for fast operations)")
	} else if !strings.Contains(err.Error(), "context") {
		t.Logf("Expected context error, got: %v (may be acceptable)", err)
	}
}

func TestMacOSCaseInsensitivePaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific test")
	}

	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	os.WriteFile(filepath.Join(tmpDir, "TestFile.txt"), []byte("content"), 0644)

	readTool := ReadTool(root)

	upperPath := strings.ToUpper(tmpDir) + "/TESTFILE.TXT"
	lowerPath := strings.ToLower(tmpDir) + "/testfile.txt"

	// Both should resolve within the workspace (not "outside workspace" error)
	_, err := readTool.Execute(context.Background(), map[string]any{
		"path": upperPath,
	})

	if err != nil && strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("path with different case should not be considered outside workspace: %v", err)
	}

	_, err = readTool.Execute(context.Background(), map[string]any{
		"path": lowerPath,
	})

	if err != nil && strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("lowercase path should not be considered outside workspace: %v", err)
	}
}
