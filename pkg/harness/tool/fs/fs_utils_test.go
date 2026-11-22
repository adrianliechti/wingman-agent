package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestEditAndReadNormalizeAbsoluteWorkspacePaths(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	absPath := filepath.Join(tmpDir, "foo", "bar.txt")
	_, err := EditTool(root).Execute(context.Background(), createArgs(absPath, "hello"))
	if err != nil {
		t.Fatalf("create absolute workspace path: %v", err)
	}

	result, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": absPath})
	if err != nil {
		t.Fatalf("read absolute workspace path: %v", err)
	}
	if !strings.Contains(result.Content.Text(), "hello") {
		t.Fatalf("read result = %q, want content", result.Content.Text())
	}
}

func TestWorkspaceBoundaryViaTools(t *testing.T) {
	root, workspace, cleanup := createTestRoot(t)
	defer cleanup()

	_, err := ReadTool(root, workspace).Execute(context.Background(), map[string]any{"file_path": "/etc/passwd"})
	if runtime.GOOS != "windows" {
		if err == nil || !strings.Contains(err.Error(), "outside workspace") {
			t.Fatalf("expected outside workspace error, got: %v", err)
		}
	}

	_, err = EditTool(root).Execute(context.Background(), createArgs(filepath.Join(os.TempDir(), "wingman-outside-test.txt"), "x"))
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("expected outside workspace create error, got: %v", err)
	}
}

func TestReadBinaryDetectionByContent(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	// Text content stays readable regardless of a "binary" extension.
	if err := os.WriteFile(filepath.Join(tmpDir, "unix.doc"), []byte("plain text documentation"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": "unix.doc"}); err != nil {
		t.Fatalf("expected text .doc to be readable, got: %v", err)
	}

	// A NUL byte marks binary content even under a text extension.
	if err := os.WriteFile(filepath.Join(tmpDir, "data.txt"), []byte("head\x00\x00tail"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": "data.txt"})
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary-content rejection, got: %v", err)
	}
}

// Reads follow the shell sandbox, which leaves the disk readable; read roots restore the confinement. Writes always need a root.
func TestOutsidePathsAreReadableUnlessConfinedAndNeverWritable(t *testing.T) {
	root, workspace, cleanup := createTestRoot(t)
	defer cleanup()

	outside := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(outside, []byte("system config"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		makeTool func(*os.Root, ...string) tool.Tool
		args     map[string]any
		want     string
	}{
		{ReadTool, map[string]any{"file_path": outside}, "system config"},
		{GrepTool, map[string]any{"path": outside, "pattern": "system"}, outside},
		{GrepTool, map[string]any{"path": filepath.Dir(outside), "pattern": "system"}, outside},
		{GlobTool, map[string]any{"path": filepath.Dir(outside), "pattern": "*.txt"}, outside},
	} {
		name := check.makeTool(root).Name
		out, err := check.makeTool(root).Execute(context.Background(), check.args)
		if err != nil || !strings.Contains(out.Content.Text(), check.want) {
			t.Errorf("%s without read roots = %q, %v; want %q", name, out.Content.Text(), err, check.want)
		}
		// "*" was once a wildcard; it must now confine like any other root that does not cover the path.
		for _, roots := range [][]string{{workspace}, {"*"}} {
			if out, err := check.makeTool(root, roots...).Execute(context.Background(), check.args); err == nil || !strings.Contains(err.Error(), "outside workspace") {
				t.Errorf("%s confined to %q = %q, %v; want the outside-workspace error", name, roots, out.Content.Text(), err)
			}
		}
	}
	for _, roots := range [][]string{nil, {"*"}} {
		_, err := EditTool(root, roots...).Execute(context.Background(), map[string]any{"file_path": outside, "old_string": "system", "new_string": "edited"})
		if err == nil || !strings.Contains(err.Error(), "outside workspace") {
			t.Errorf("edit with write roots %q = %v, want the outside-workspace error", roots, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "system config" {
		t.Fatalf("outside file = %q, %v", data, err)
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
		"file_path":  "bom_crlf.txt",
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
		"file_path":  "typography.txt",
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
	if !strings.Contains(string(content), `say “bye”`) || !strings.Contains(string(content), "foo-baz") {
		t.Fatalf("unexpected edited content: %q", string(content))
	}
}

func TestEditReturnsConciseFileSummary(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(tmpDir, "diff.txt"), []byte("line1\nold\nline3"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := EditTool(root).Execute(context.Background(), map[string]any{
		"file_path":  "diff.txt",
		"old_string": "old",
		"new_string": "new",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if result.Content.Text() != "Applied 1 edit across 1 file.\nM diff.txt" {
		t.Fatalf("unexpected edit summary: %q", result.Content.Text())
	}
}
