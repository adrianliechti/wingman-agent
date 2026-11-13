package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestEditDeleteMoveAndUndo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.go"), []byte("package app\n\nfunc Legacy() string { return \"old\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "obsolete.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	fresh := NewFreshness(root)
	tools := Tools(root, &Options{Freshness: fresh})
	var readTool, editTool tool.Tool
	for _, tl := range tools {
		switch tl.Name {
		case "read":
			readTool = tl
		case "edit":
			editTool = tl
		}
	}

	// Read both files first to satisfy freshness tracking.
	if _, err := readTool.Execute(t.Context(), map[string]any{"file_path": "legacy.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readTool.Execute(t.Context(), map[string]any{"file_path": "obsolete.txt"}); err != nil {
		t.Fatal(err)
	}

	// Batch: move+edit legacy.go -> internal/modern.go AND delete obsolete.txt.
	res, err := editTool.Execute(t.Context(), map[string]any{
		"edits": []any{
			map[string]any{
				"file_path":  "legacy.go",
				"move_to":    "internal/modern.go",
				"old_string": "func Legacy() string { return \"old\" }",
				"new_string": "func Modern() string { return \"new\" }",
			},
			map[string]any{
				"file_path": "obsolete.txt",
				"delete":    true,
			},
		},
	})
	if err != nil {
		t.Fatalf("edit move+delete failed: %v", err)
	}
	if !strings.Contains(res.Content.Text(), "R legacy.go -> internal/modern.go") || !strings.Contains(res.Content.Text(), "D obsolete.txt") {
		t.Fatalf("unexpected edit summary:\n%s", res.Content.Text())
	}

	if _, err := os.Stat(filepath.Join(dir, "legacy.go")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy.go to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected obsolete.txt to be removed, stat err = %v", err)
	}
	gotModern, err := os.ReadFile(filepath.Join(dir, "internal", "modern.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotModern), "func Modern()") {
		t.Fatalf("unexpected moved content: %s", string(gotModern))
	}

	// UndoFileChanges must restore legacy.go and obsolete.txt and remove internal/modern.go.
	changes, err := tool.ResultFileChanges(res.Metadata)
	if err != nil || len(changes) != 3 {
		t.Fatalf("expected 3 file changes (src removal, dst creation, delete), got %d (%v)", len(changes), err)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatalf("UndoFileChanges failed: %v", err)
	}

	restoredLegacy, err := os.ReadFile(filepath.Join(dir, "legacy.go"))
	if err != nil || !strings.Contains(string(restoredLegacy), "func Legacy()") {
		t.Fatalf("legacy.go not restored: %v / %q", err, string(restoredLegacy))
	}
	restoredObsolete, err := os.ReadFile(filepath.Join(dir, "obsolete.txt"))
	if err != nil || string(restoredObsolete) != "remove me\n" {
		t.Fatalf("obsolete.txt not restored: %v / %q", err, string(restoredObsolete))
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "modern.go")); !os.IsNotExist(err) {
		t.Fatalf("expected internal/modern.go to be removed on undo, stat err = %v", err)
	}
}

func TestEditMoveRejectsExistingDestinationWithoutPartialChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	editTool := EditTool(root)
	_, err = editTool.Execute(t.Context(), map[string]any{
		"edits": []any{
			map[string]any{
				"file_path": "a.txt",
				"move_to":   "b.txt",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("expected destination collision error, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "alpha\n" {
		t.Fatalf("a.txt modified despite failed batch: %q", string(got))
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "beta\n" {
		t.Fatalf("b.txt modified despite failed batch: %q", string(got))
	}
}

func TestWrapWithWorkspaceSnapshotTracksExecMutationsAndUndo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	fakeExec := tool.Tool{
		Name:   "exec",
		Effect: tool.StaticEffect(tool.EffectMutates),
		Execute: func(_ context.Context, _ map[string]any) (tool.Result, error) {
			// Simulate gofmt + codegen + ignored build output.
			_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "generated.go"), []byte("package main\n\nconst Version = \"1.0\"\n"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "dist", "bundle.js"), []byte("ignored"), 0o644)
			return tool.Result{Content: tool.TextContent("ok"), Metadata: map[string]any{"exit_code": 0}}, nil
		},
	}

	wrapped := WrapWithWorkspaceSnapshot(root, []tool.Tool{fakeExec})[0]
	res, err := wrapped.Execute(t.Context(), map[string]any{"command": "gofmt -w . && go generate ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if complete, _ := res.Metadata["workspace_snapshot_complete"].(bool); !complete {
		t.Fatalf("expected complete workspace snapshot metadata, got %+v", res.Metadata)
	}
	changes, err := tool.ResultFileChanges(res.Metadata)
	if err != nil || len(changes) != 2 {
		t.Fatalf("expected 2 non-ignored file changes, got %d (%+v, err=%v)", len(changes), changes, err)
	}

	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatalf("UndoFileChanges failed: %v", err)
	}
	gotMain, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || string(gotMain) != "package main\nfunc main(){}\n" {
		t.Fatalf("main.go not restored after exec undo: %v / %q", err, string(gotMain))
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.go")); !os.IsNotExist(err) {
		t.Fatalf("expected generated.go to be removed after exec undo, err=%v", err)
	}
}

func TestMoveOverDeletedDestinationPreservesModesAndUndo(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, mode := range map[string]os.FileMode{"source": 0755, "destination": 0644} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("same contents"), mode); err != nil {
			t.Fatal(err)
		}
	}
	result, err := EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": "destination", "delete": true}, map[string]any{"file_path": "source", "move_to": "destination"}}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "destination"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("move lost executable mode: %v %v", info, err)
	}
	changes, err := tool.ResultFileChanges(result.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"source": 0755, "destination": 0644} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("undo mode %s: %v %v", name, info, err)
		}
	}
}

// Optional fields arrive as null, false, or "" from models that fill every
// schema property. Those spellings must not change what the entry does.
func TestEditOptionalFieldSpellings(t *testing.T) {
	const original = "package a\n\nfunc A() int { return 1 }\n"
	cases := []struct {
		name       string
		entry      map[string]any
		wantA      string // "" means a.go must be gone
		wantB      string // "" means b.go must not exist
		wantErrHas string
	}{
		{"move with fields omitted", map[string]any{"file_path": "a.go", "move_to": "b.go"}, "", original, ""},
		{"move with empty strings", map[string]any{"file_path": "a.go", "move_to": "b.go", "old_string": "", "new_string": ""}, "", original, ""},
		{"move with null strings", map[string]any{"file_path": "a.go", "move_to": "b.go", "old_string": nil, "new_string": nil, "delete": false, "replace_all": nil}, "", original, ""},
		{"move and edit", map[string]any{"file_path": "a.go", "move_to": "b.go", "old_string": "return 1", "new_string": "return 2"}, "", "package a\n\nfunc A() int { return 2 }\n", ""},
		{"edit with empty move_to", map[string]any{"file_path": "a.go", "old_string": "return 1", "new_string": "return 2", "move_to": ""}, "package a\n\nfunc A() int { return 2 }\n", "", ""},
		{"edit with null extras", map[string]any{"file_path": "a.go", "old_string": "return 1", "new_string": "return 2", "move_to": nil, "delete": nil, "replace_all": nil}, "package a\n\nfunc A() int { return 2 }\n", "", ""},
		{"delete with empty strings", map[string]any{"file_path": "a.go", "delete": true, "old_string": "", "new_string": nil, "move_to": ""}, "", "", ""},
		{"delete with replacement text", map[string]any{"file_path": "a.go", "delete": true, "old_string": "return 1"}, original, "", "delete cannot be combined with old_string"},
		{"delete and move", map[string]any{"file_path": "a.go", "delete": true, "move_to": "b.go"}, original, "", "delete cannot be combined with move_to"},
		{"wrong type", map[string]any{"file_path": "a.go", "move_to": 7}, original, "", "move_to must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			_, err = EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{tc.entry}})
			if tc.wantErrHas == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if tc.wantErrHas != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrHas)) {
				t.Fatalf("error = %v, want %q", err, tc.wantErrHas)
			}
			for name, want := range map[string]string{"a.go": tc.wantA, "b.go": tc.wantB} {
				got, readErr := os.ReadFile(filepath.Join(dir, name))
				if want == "" && !os.IsNotExist(readErr) {
					t.Fatalf("%s should not exist, got %q (%v)", name, got, readErr)
				}
				if want != "" && string(got) != want {
					t.Fatalf("%s = %q, want %q (%v)", name, got, want, readErr)
				}
			}
		})
	}
}
