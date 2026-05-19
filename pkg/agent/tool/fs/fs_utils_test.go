package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

func TestWriteAndReadNormalizeAbsoluteWorkspacePaths(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	absPath := filepath.Join(tmpDir, "foo", "bar.txt")
	_, err := WriteTool(root).Execute(context.Background(), map[string]any{
		"path":    absPath,
		"content": "hello",
	})
	if err != nil {
		t.Fatalf("write absolute workspace path: %v", err)
	}

	result, err := ReadTool(root).Execute(context.Background(), map[string]any{"path": absPath})
	if err != nil {
		t.Fatalf("read absolute workspace path: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Fatalf("read result = %q, want content", result)
	}
}

func TestWorkspaceBoundaryViaTools(t *testing.T) {
	root, _, cleanup := createTestRoot(t)
	defer cleanup()

	_, err := ReadTool(root).Execute(context.Background(), map[string]any{"path": "/etc/passwd"})
	if runtime.GOOS != "windows" {
		if err == nil || !strings.Contains(err.Error(), "outside workspace") {
			t.Fatalf("expected outside workspace error, got: %v", err)
		}
	}

	_, err = WriteTool(root).Execute(context.Background(), map[string]any{
		"path":    filepath.Join(os.TempDir(), "wingman-outside-test.txt"),
		"content": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("expected outside workspace write error, got: %v", err)
	}
}

func TestReadRejectsBinaryExtensions(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(tmpDir, "image.PNG"), []byte("data"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := ReadTool(root).Execute(context.Background(), map[string]any{"path": "image.PNG"})
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary-file rejection, got: %v", err)
	}
}

func TestEditPreservesBOMAndLineEndings(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "bom_crlf.txt")
	if err := os.WriteFile(path, []byte("\uFEFFline1\r\nline2\r\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := EditTool(root).Execute(context.Background(), map[string]any{
		"path":       "bom_crlf.txt",
		"old_string": "line2",
		"new_string": "changed",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if !strings.HasPrefix(string(content), "\uFEFF") {
		t.Fatalf("BOM was not preserved: %q", string(content))
	}
	if !strings.Contains(string(content), "\r\n") {
		t.Fatalf("CRLF line endings were not preserved: %q", string(content))
	}
}

func TestEditFuzzyMatchesCommonTypography(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "typography.txt")
	if err := os.WriteFile(path, []byte("say “hello”\nfoo—bar\nhello\u00A0world\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := EditTool(root).Execute(context.Background(), map[string]any{
		"path":       "typography.txt",
		"old_string": "say \"hello\"\nfoo-bar\nhello world",
		"new_string": "say \"bye\"\nfoo-baz\nhello world",
	})
	if err != nil {
		t.Fatalf("fuzzy edit: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if !strings.Contains(string(content), `say "bye"`) || !strings.Contains(string(content), "foo-baz") {
		t.Fatalf("unexpected edited content: %q", string(content))
	}
}

func TestEditReturnsLineDiff(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(tmpDir, "diff.txt"), []byte("line1\nold\nline3"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := EditTool(root).Execute(context.Background(), map[string]any{
		"path":       "diff.txt",
		"old_string": "old",
		"new_string": "new",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(result, "-2 old") || !strings.Contains(result, "+2 new") {
		t.Fatalf("expected line-numbered diff, got: %q", result)
	}
}
