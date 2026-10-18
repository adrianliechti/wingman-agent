package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditRejectsProtectedPathsBeforeWritingBatch(t *testing.T) {
	for _, protected := range []string{".git/config", ".git", "sub/.git/config", ".agents/new.md", ".vscode/settings.json", "mcp.json", ".envrc"} {
		t.Run(protected, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			args := map[string]any{"edits": []any{
				map[string]any{"file_path": "ordinary.txt", "old_string": "", "new_string": "ordinary"},
				map[string]any{"file_path": protected, "old_string": "", "new_string": "protected"},
			}}
			if _, err := EditTool(root).Execute(t.Context(), args); err == nil || !strings.Contains(err.Error(), "protected") {
				t.Fatalf("edit = %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "ordinary.txt")); !os.IsNotExist(err) {
				t.Fatal("partial batch wrote ordinary file")
			}
		})
	}
}

func TestEditRejectsProtectedPathThroughSymlink(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		dir := t.TempDir()
		target, alias := ".git", "alias"
		if reverse {
			target, alias = alias, target
		}
		if err := os.Mkdir(filepath.Join(dir, target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, alias)); err != nil {
			t.Skip(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"edits": []any{map[string]any{"file_path": "alias/config", "old_string": "", "new_string": "payload"}}}
		_, err = EditTool(root).Execute(t.Context(), args)
		root.Close()
		if err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("reverse=%t: %v", reverse, err)
		}
	}
}

func TestEditStillRejectsProtectedFileWhenShellSandboxDisabled(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": ".git/config", "old_string": "", "new_string": "fixture"}}})
	if err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("edit = %v", err)
	}
}
