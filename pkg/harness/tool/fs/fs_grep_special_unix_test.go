//go:build unix

package fs

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGrepRejectsExplicitPipesAndSkipsThemDuringTraversal(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "needle"})
	if err := unix.Mkfifo(filepath.Join(root.Name(), "events.pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": "events.pipe", "pattern": "needle"})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v", err)
	}
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if err != nil || result.Content != "Found 1 file\nfile" {
		t.Fatalf("result = %q, %v", result.Content, err)
	}
}
