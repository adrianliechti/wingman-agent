package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestSnapshotDoesNotInventCreationOrDeletion(t *testing.T) {
	for _, scenario := range []string{"binary_to_text", "text_to_binary", "unignore", "ignore"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before := []byte("original\n")
			if scenario == "binary_to_text" {
				before = []byte{0, 1, 2}
			}
			if err := os.WriteFile(filepath.Join(dir, "file"), before, 0644); err != nil {
				t.Fatal(err)
			}
			if scenario == "unignore" {
				if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("file\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			wrapped := WrapWithWorkspaceSnapshot(root, []tool.Tool{{
				Name: "exec", Effect: tool.StaticEffect(tool.EffectMutates),
				Execute: func(context.Context, map[string]any) (tool.Result, error) {
					var err error
					switch scenario {
					case "binary_to_text":
						err = os.WriteFile(filepath.Join(dir, "file"), []byte("now text\n"), 0644)
					case "text_to_binary":
						err = os.WriteFile(filepath.Join(dir, "file"), []byte{0, 1, 2}, 0644)
					case "unignore":
						err = os.WriteFile(filepath.Join(dir, ".gitignore"), nil, 0644)
					case "ignore":
						err = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("file\n"), 0644)
					}
					return tool.Result{Metadata: map[string]any{"exit_code": 0}}, err
				},
			}})[0]
			result, err := wrapped.Execute(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			changes, err := tool.ResultFileChanges(result.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range changes {
				if change.Path == "file" {
					t.Fatalf("unsafe change recorded: %+v", change)
				}
			}
			if complete, _ := result.Metadata["workspace_snapshot_complete"].(bool); complete {
				t.Fatal("claimed complete tracking")
			}
		})
	}
}

func TestIncompleteSnapshotDoesNotInventMissingFiles(t *testing.T) {
	before := &WorkspaceSnapshot{files: map[string]snapshotEntry{"file": {content: []byte("original")}}, complete: true}
	after := &WorkspaceSnapshot{files: map[string]snapshotEntry{}, complete: false}
	for _, pair := range [][2]*WorkspaceSnapshot{{before, after}, {after, before}} {
		changes, complete := pair[0].Diff(pair[1])
		if len(changes) != 0 || complete {
			t.Fatalf("changes=%+v complete=%v", changes, complete)
		}
	}
}

func TestSnapshotTracksNewTextAlongsideSkippedBinary(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "binary"), []byte{0, 1, 2}, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := CaptureWorkspaceSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureWorkspaceSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	changes, complete := before.Diff(after)
	if complete || len(changes) != 1 || changes[0].Path != "new.go" || changes[0].BeforeExists {
		t.Fatalf("changes=%+v complete=%v", changes, complete)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.go")); !os.IsNotExist(err) {
		t.Fatal("created text file not undone")
	}
}

func TestSnapshotTracksAndUndoesModeChanges(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("script", []byte("echo ok"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := CaptureWorkspaceSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Chmod("script", 0755); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureWorkspaceSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	changes, complete := before.Diff(after)
	if !complete || len(changes) != 1 || changes[0].ResultMode() != 0755 {
		t.Fatalf("mode tracking: %+v", changes)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat("script")
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("mode not restored: %v %v", info, err)
	}
}

// A command that rewrites a file the model already read must still trigger the
// changed-on-disk notice and the stale-edit guard: the model has not seen the
// new content, so tracking the change must not count as reading it.
func TestExecSnapshotKeepsFilesStaleUntilReread(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	fresh := NewFreshness(root)
	find := func(name string) tool.Tool {
		for _, candidate := range Tools(root, &Options{Freshness: fresh}) {
			if candidate.Name == name {
				return candidate
			}
		}
		t.Fatalf("tool %s missing", name)
		return tool.Tool{}
	}
	if _, err := find("read").Execute(t.Context(), map[string]any{"file_path": "a.go"}); err != nil {
		t.Fatal(err)
	}

	formatter := WrapWithWorkspaceSnapshot(root, []tool.Tool{{
		Name: "exec", Effect: tool.StaticEffect(tool.EffectMutates),
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text("ok"), os.WriteFile(path, []byte("package a\n\nfunc A() int {\n\treturn 1 // reformatted\n}\n"), 0o644)
		},
	}})[0]
	result, err := formatter.Execute(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if changes, err := tool.ResultFileChanges(result.Metadata); err != nil || len(changes) != 1 {
		t.Fatalf("command change not tracked: %+v %v", changes, err)
	}

	if changed := fresh.Changed(); len(changed) != 1 {
		t.Fatalf("changed-on-disk notice suppressed: %v", changed)
	}
	edit := map[string]any{"edits": []any{map[string]any{"file_path": "a.go", "old_string": "return 1", "new_string": "return 2"}}}
	if _, err := find("edit").Execute(t.Context(), edit); err == nil || !strings.Contains(err.Error(), "read it again") {
		t.Fatalf("stale edit accepted after a command rewrote the file: %v", err)
	}
	if _, err := find("read").Execute(t.Context(), map[string]any{"file_path": "a.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := find("edit").Execute(t.Context(), edit); err != nil {
		t.Fatalf("edit after re-read rejected: %v", err)
	}
}
