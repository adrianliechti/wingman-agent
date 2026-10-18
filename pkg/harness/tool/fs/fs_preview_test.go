package fs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestPrivatePreviewSnapshotGuardsEntireAtomicBatch(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := "old\noriginal\n"
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
	}
	freshness := NewFreshness(root)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(before)))
	preview := freshness.TrackSnapshots(tool.Tool{Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Result{Content: "preview", Metadata: map[string]any{tool.FileSnapshotsMetadata: []tool.FileSnapshot{
			{Path: "a.txt", SHA256: digest}, {Path: "b.txt", SHA256: digest},
		}}}, nil
	}})
	if _, err := preview.Execute(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	edit := batchEditTool(root, freshness)
	encoded, err := json.Marshal(edit.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sha256") {
		t.Fatal("private digest leaked into the edit schema")
	}
	args := map[string]any{"edits": []any{
		map[string]any{"file_path": "a.txt", "old_string": "old", "new_string": "new"},
		map[string]any{"file_path": "b.txt", "old_string": "old", "new_string": "new"},
	}}
	// Preserve size and mtime: neither stat freshness nor old_string catches it.
	path := filepath.Join(dir, "b.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\nexternal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := edit.Execute(t.Context(), args); err == nil || !strings.Contains(err.Error(), "content changed since the preview") {
		t.Fatalf("stale preview accepted: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != before {
		t.Fatal("atomic batch changed its first file")
	}
	// A fresh ordinary read advances the baseline deliberately.
	target, err := resolveFileTarget("b.txt", dir, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	freshness.record(t.Context(), target)
	if _, err := edit.Execute(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new\nexternal\n" {
		t.Fatalf("external edit lost: %q", data)
	}
}

func TestPrivatePreviewSnapshotsKeepAgentBaselinesSeparate(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	freshness := NewFreshness(root)
	preview := freshness.TrackSnapshots(tool.Tool{Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Result{Metadata: map[string]any{tool.FileSnapshotsMetadata: []tool.FileSnapshot{{Path: "a.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("original")))}}}}, nil
	}})
	background := tool.WithBackgroundOrigin(t.Context(), "worker")
	if _, err := preview.Execute(background, nil); err != nil {
		t.Fatal(err)
	}
	target, err := resolveFileTarget("a.txt", dir, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if freshness.staleContent(t.Context(), target, []byte("changed!")) {
		t.Fatal("background preview changed the parent's baseline")
	}
	if !freshness.staleContent(background, target, []byte("changed!")) {
		t.Fatal("background snapshot not recorded")
	}
	freshness.record(t.Context(), target)
	if !freshness.staleContent(background, target, []byte("changed!")) {
		t.Fatal("parent read erased background snapshot")
	}
}
