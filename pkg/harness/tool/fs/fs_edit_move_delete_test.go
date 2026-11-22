package fs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

func TestEditMoveAndDeletePruneEmptiedDirectories(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"internal/util/slug.go":        "package util\n",
		"internal/util/strings.go":     "package util\n",
		"internal/legacy/v1/format.go": "package v1\n",
		"internal/keep/kept.go":        "package keep\n",
		"internal/keep/removed.go":     "package keep\n",
		"top.txt":                      "top\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	var readTool, editTool tool.Tool
	for _, tl := range Tools(root, &Options{Freshness: NewFreshness(root)}) {
		switch tl.Name {
		case "read":
			readTool = tl
		case "edit":
			editTool = tl
		}
	}
	for name := range files {
		if _, err := readTool.Execute(t.Context(), map[string]any{"file_path": name}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := editTool.Execute(t.Context(), map[string]any{"edits": []any{
		map[string]any{"file_path": "internal/util/slug.go", "move_to": "internal/textutil/slug.go", "old_string": "package util", "new_string": "package textutil"},
		map[string]any{"file_path": "internal/util/strings.go", "move_to": "internal/textutil/strings.go", "old_string": "package util", "new_string": "package textutil"},
		map[string]any{"file_path": "internal/legacy/v1/format.go", "delete": true},
		map[string]any{"file_path": "internal/keep/removed.go", "delete": true},
		map[string]any{"file_path": "top.txt", "delete": true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"internal/util", "internal/legacy/v1", "internal/legacy"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s was left behind: %v", gone, err)
		}
		if !strings.Contains(res.Content.Text(), gone) {
			t.Errorf("result does not report removing %s:\n%s", gone, res.Content.Text())
		}
	}
	// A directory that still holds a file, and the root itself, must survive.
	for _, kept := range []string{"internal/keep/kept.go", "internal/textutil/slug.go", "."} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s is missing: %v", kept, err)
		}
	}
	if strings.Contains(res.Content.Text(), "internal/keep") && !strings.Contains(res.Content.Text(), "D internal/keep/removed.go") {
		t.Errorf("result reports a directory that was not removed:\n%s", res.Content.Text())
	}

	changes, err := tool.ResultFileChanges(res.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Errorf("undo did not restore %s: %q, %v", name, got, err)
		}
	}
	// Undo removes the files the edit created, and with them their directory.
	if _, err := os.Stat(filepath.Join(dir, "internal", "textutil")); !os.IsNotExist(err) {
		t.Errorf("undo left internal/textutil behind: %v", err)
	}
}

// Inkling and DeepSeek Flash dropped an optional new_string in live probes; providers keep required arguments.
func TestEditSchemaRequiresBothStrings(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	edits := EditTool(root).Parameters["properties"].(map[string]any)["edits"].(map[string]any)
	required, _ := edits["items"].(map[string]any)["required"].([]string)
	for _, want := range []string{"file_path", "old_string", "new_string"} {
		if !slices.Contains(required, want) {
			t.Errorf("edit entries do not require %s: %v", want, required)
		}
	}
}

// Inkling sends two empty strings when it means to delete; the error has to name the way out.
func TestEditEmptyStringsOnExistingFilePointToDeleteTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	_, err = EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": "a.go", "old_string": "", "new_string": "", "replace_all": false}}})
	if err == nil || !strings.Contains(err.Error(), "use the `delete` tool") {
		t.Fatalf("error = %v, want it to point to the delete tool", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(got) != "package a\n" {
		t.Fatalf("a.go changed to %q", got)
	}
}

func TestDeleteToolRemovesListedFilesOrNone(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"pkg/a.go": "package pkg\n", "pkg/b.go": "package pkg\n", "keep.go": "package main\n"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	remove := DeleteTool(root)

	// One missing path fails the call before anything is removed, and the error names the caller's argument.
	_, err = remove.Execute(t.Context(), map[string]any{"file_paths": []any{"pkg/a.go", "pkg/missing.go"}})
	if err == nil || !strings.Contains(err.Error(), "file_paths[1]") || strings.Contains(err.Error(), "edits[") {
		t.Fatalf("error = %v, want it to name file_paths[1]", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pkg", "a.go")); statErr != nil {
		t.Fatalf("pkg/a.go was removed by a failed call: %v", statErr)
	}
	// Diagnostics should preserve a literal filename that resembles an edit field.
	_, err = remove.Execute(t.Context(), map[string]any{"file_paths": []any{"edits[notes].txt"}})
	if err == nil || !strings.Contains(err.Error(), "file_paths[0]") || !strings.Contains(err.Error(), "edits[notes].txt") {
		t.Fatalf("delete changed the filename in its diagnostic: %v", err)
	}

	result, err := remove.Execute(t.Context(), map[string]any{"file_paths": []any{"pkg/a.go", "pkg/b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content.Text(); !strings.HasPrefix(text, "Deleted 2 files.") || !strings.Contains(text, "pkg/a.go") {
		t.Fatalf("result = %q", text)
	}
	if changes, changesErr := tool.ResultFileChanges(result.Metadata); changesErr != nil || len(changes) != 2 {
		t.Fatalf("file changes = %v, %v; want the two deletions for review and undo", changes, changesErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pkg")); !os.IsNotExist(statErr) {
		t.Fatalf("emptied directory still exists: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "keep.go")); statErr != nil {
		t.Fatalf("unlisted file was removed: %v", statErr)
	}

	for name, args := range map[string]map[string]any{
		"empty list": {"file_paths": []any{}},
		"pattern":    {"file_paths": []any{"*.go"}},
		"directory":  {"file_paths": []any{"."}},
		"outside":    {"file_paths": []any{"../elsewhere.go"}},
	} {
		if _, err := remove.Execute(t.Context(), args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "keep.go")); statErr != nil {
		t.Fatalf("keep.go was removed by a rejected call: %v", statErr)
	}
}

func TestMoveAndDeletePreserveFileSymlinksAndTheirTargets(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if err := root.WriteFile("target.txt", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("target.txt", "alias.txt"); err != nil {
		t.Skipf("file links unavailable: %v", err)
	}
	for _, tc := range []struct {
		tool tool.Tool
		args map[string]any
	}{
		{DeleteTool(root), map[string]any{"file_paths": []any{"alias.txt"}}},
		{EditTool(root), map[string]any{"file_path": "alias.txt", "move_to": "moved.txt"}},
		{EditTool(root), map[string]any{"file_path": "alias.txt", "delete": true}},
	} {
		if _, err := tc.tool.Execute(t.Context(), tc.args); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s returned %v", tc.tool.Name, err)
		}
	}
	if dest, err := root.Readlink("alias.txt"); err != nil || dest != "target.txt" {
		t.Fatalf("alias = %q, %v", dest, err)
	}
	if data, err := root.ReadFile("target.txt"); err != nil || string(data) != "original" {
		t.Fatalf("target = %q, %v", data, err)
	}
	if _, err := root.Stat("moved.txt"); !os.IsNotExist(err) {
		t.Fatalf("move created a destination: %v", err)
	}
}

func TestPruningSkipsAParentReplacedWithAnOrdinaryFile(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if err := root.WriteFile("old-dir", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	change := fileChange{root: root, path: filepath.Join("old-dir", "removed"), target: fileTarget{InWorkspace: true}}
	if pruned := pruneEmptyParents(&change); len(pruned) != 0 {
		t.Fatalf("reported pruned directories: %v", pruned)
	}
	if b, err := root.ReadFile("old-dir"); err != nil || string(b) != "keep" {
		t.Fatalf("pruning removed the replacement: %q, %v", b, err)
	}
}

// Sessions saved before the split replay entries that carry the flag.
func TestEditStillAcceptsTheDeleteFlagButDoesNotAdvertiseIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	edit := EditTool(root)
	entry := edit.Parameters["properties"].(map[string]any)["edits"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, advertised := entry["delete"]; advertised || strings.Contains(edit.Description, "delete: true") {
		t.Fatal("the edit schema still offers a delete flag")
	}
	if _, err := edit.Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": "a.go", "delete": true}}}); err != nil {
		t.Fatalf("an entry with the delete flag was rejected: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.go")); !os.IsNotExist(statErr) {
		t.Fatalf("a.go still exists: %v", statErr)
	}
}
