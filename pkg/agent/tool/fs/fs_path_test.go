package fs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
