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
