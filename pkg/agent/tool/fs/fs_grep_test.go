package fs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
