package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

// createWriteRootSetup returns (workspaceRoot, workspaceDir, allowedDir, cleanup).
// allowedDir is a sibling of workspaceDir intended to be passed as an
// allowedWriteRoot — i.e. outside the workspace.
func createWriteRootSetup(t *testing.T) (*os.Root, string, string, func()) {
	t.Helper()

	parent, err := os.MkdirTemp("", "fs_write_root_*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}

	workspaceDir := filepath.Join(parent, "workspace")
	allowedDir := filepath.Join(parent, "memory")
	for _, d := range []string{workspaceDir, allowedDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			os.RemoveAll(parent)
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		os.RemoveAll(parent)
		t.Fatalf("open root: %v", err)
	}

	cleanup := func() {
		root.Close()
		os.RemoveAll(parent)
	}
	return root, workspaceDir, allowedDir, cleanup
}

func TestWriteToolAllowsWritesInsideAllowedRoot(t *testing.T) {
	root, _, allowedDir, cleanup := createWriteRootSetup(t)
	defer cleanup()

	writeTool := WriteTool(root, allowedDir)

	target := filepath.Join(allowedDir, "feedback_testing.md")
	if _, err := writeTool.Execute(context.Background(), map[string]any{
		"path":    target,
		"content": "hello memory",
	}); err != nil {
		t.Fatalf("write inside allowed root: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "hello memory" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestWriteToolRejectsWritesOutsideAllowedRoots(t *testing.T) {
	root, _, allowedDir, cleanup := createWriteRootSetup(t)
	defer cleanup()

	writeTool := WriteTool(root, allowedDir)

	other, err := os.MkdirTemp("", "fs_other_*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(other)

	_, err = writeTool.Execute(context.Background(), map[string]any{
		"path":    filepath.Join(other, "x.md"),
		"content": "nope",
	})
	if err == nil {
		t.Fatal("expected error writing outside workspace and allowed roots")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("expected sandbox error, got: %v", err)
	}
}

func TestEditToolWorksInsideAllowedRoot(t *testing.T) {
	root, _, allowedDir, cleanup := createWriteRootSetup(t)
	defer cleanup()

	target := filepath.Join(allowedDir, "MEMORY.md")
	if err := os.WriteFile(target, []byte("- old line\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	editTool := EditTool(root, allowedDir)
	if _, err := editTool.Execute(context.Background(), map[string]any{
		"path":       target,
		"old_string": "old line",
		"new_string": "new line",
	}); err != nil {
		t.Fatalf("edit inside allowed root: %v", err)
	}

	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "new line") {
		t.Errorf("edit did not apply: %s", data)
	}
}

func TestEditToolRejectsEditsOutsideAllowedRoots(t *testing.T) {
	root, _, allowedDir, cleanup := createWriteRootSetup(t)
	defer cleanup()

	editTool := EditTool(root, allowedDir)

	other, err := os.MkdirTemp("", "fs_other_*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(other)

	target := filepath.Join(other, "x.md")
	if err := os.WriteFile(target, []byte("seed"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = editTool.Execute(context.Background(), map[string]any{
		"path":       target,
		"old_string": "seed",
		"new_string": "tampered",
	})
	if err == nil {
		t.Fatal("expected error editing outside workspace and allowed roots")
	}
}

func TestWriteToolWithoutAllowedRootsStillRespectsWorkspace(t *testing.T) {
	root, workspaceDir, _, cleanup := createWriteRootSetup(t)
	defer cleanup()

	// No allowed write roots passed — should behave like the old API.
	writeTool := WriteTool(root)

	rel := "in_workspace.txt"
	if _, err := writeTool.Execute(context.Background(), map[string]any{
		"path":    rel,
		"content": "in workspace",
	}); err != nil {
		t.Fatalf("workspace write: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(workspaceDir, rel))
	if string(data) != "in workspace" {
		t.Errorf("workspace write did not land: %q", data)
	}

	// Sibling dir must still be rejected when no write roots are configured.
	sibling, err := os.MkdirTemp("", "fs_sib_*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(sibling)

	if _, err := writeTool.Execute(context.Background(), map[string]any{
		"path":    filepath.Join(sibling, "x.txt"),
		"content": "nope",
	}); err == nil {
		t.Fatal("expected sandbox rejection for sibling dir")
	}
}
