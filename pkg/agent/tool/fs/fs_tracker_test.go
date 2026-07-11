package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

func trackedTools(t *testing.T) (map[string]tool.Tool, string, func()) {
	t.Helper()

	root, tmpDir, cleanup := createTestRoot(t)

	tools := map[string]tool.Tool{}
	for _, tl := range fs.Tools(root, nil) {
		tools[tl.Name] = tl
	}

	return tools, tmpDir, cleanup
}

func TestWriteRequiresReadForExisting(t *testing.T) {
	tools, tmpDir, cleanup := trackedTools(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "existing.txt")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "clobbered",
	})
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected read-first error, got: %v", err)
	}

	if _, err := tools["read"].Execute(context.Background(), map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "replaced",
	}); err != nil {
		t.Fatalf("overwrite after read failed: %v", err)
	}

	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "replaced again",
	}); err != nil {
		t.Fatalf("overwrite after own write failed: %v", err)
	}
}

func TestWriteRejectsExternallyModifiedFile(t *testing.T) {
	tools, tmpDir, cleanup := trackedTools(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := tools["read"].Execute(context.Background(), map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("changed externally"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "rewrite",
	})
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected stale-content error, got: %v", err)
	}

	if _, err := tools["read"].Execute(context.Background(), map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "rewrite",
	}); err != nil {
		t.Fatalf("overwrite after re-read failed: %v", err)
	}
}

func TestWriteAfterEditNeedsNoRead(t *testing.T) {
	tools, tmpDir, cleanup := trackedTools(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := tools["edit"].Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "hello",
		"new_string": "goodbye",
	}); err != nil {
		t.Fatalf("edit without read failed: %v", err)
	}

	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "full rewrite",
	}); err != nil {
		t.Fatalf("overwrite after own edit failed: %v", err)
	}
}

func TestWriteGuardExemptions(t *testing.T) {
	tools, tmpDir, cleanup := trackedTools(t)
	defer cleanup()

	empty := filepath.Join(tmpDir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": empty,
		"content":   "filled",
	}); err != nil {
		t.Fatalf("overwrite of empty file failed: %v", err)
	}

	binary := filepath.Join(tmpDir, "image.png")
	if err := os.WriteFile(binary, []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00}, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tools["write"].Execute(context.Background(), map[string]any{
		"file_path": binary,
		"content":   "regenerated",
	}); err != nil {
		t.Fatalf("overwrite of binary file failed: %v", err)
	}
}
