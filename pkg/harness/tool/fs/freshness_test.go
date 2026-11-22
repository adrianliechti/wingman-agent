package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestFreshnessDetectsExternalChangesOnce(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "watched.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0644); err != nil {
		t.Fatal(err)
	}

	freshness := fs.NewFreshness(root)
	tools := fs.Tools(root, &fs.Options{
		AllowedReadRoots:  []string{tmpDir},
		AllowedWriteRoots: []string{tmpDir},
		Freshness:         freshness,
	})
	byName := map[string]func(map[string]any) (string, error){}
	for _, tl := range tools {
		byName[tl.Name] = func(args map[string]any) (string, error) {
			result, err := tl.Execute(t.Context(), args)
			return result.Content.Text(), err
		}
	}

	if _, err := byName["read"](map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("changed after own read = %v", changed)
	}

	if err := os.WriteFile(path, []byte("edited externally\n"), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(2 * time.Second)
	os.Chtimes(path, past, past)

	changed := freshness.Changed()
	if len(changed) != 1 || changed[0] != path {
		t.Fatalf("changed = %v, want [%s]", changed, path)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("second sweep = %v, want empty", changed)
	}

	if _, err := byName["read"](map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["edit"](map[string]any{
		"file_path": path, "old_string": "edited externally", "new_string": "edited by tool",
	}); err != nil {
		t.Fatal(err)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("changed after own edit = %v", changed)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	changed = freshness.Changed()
	if len(changed) != 1 || changed[0] != path+" (deleted)" {
		t.Fatalf("changed after delete = %v", changed)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("sweep after delete = %v, want empty", changed)
	}
}

func TestEditRejectsStaleFileAfterExternalChange(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	path := filepath.Join(tmpDir, "stale.txt")
	os.WriteFile(path, []byte("alpha beta\n"), 0644)

	freshness := fs.NewFreshness(root)
	tools := fs.Tools(root, &fs.Options{
		AllowedReadRoots:  []string{tmpDir},
		AllowedWriteRoots: []string{tmpDir},
		Freshness:         freshness,
	})
	byName := map[string]func(map[string]any) (string, error){}
	for _, tl := range tools {
		byName[tl.Name] = func(args map[string]any) (string, error) {
			result, err := tl.Execute(t.Context(), args)
			return result.Content.Text(), err
		}
	}

	if _, err := byName["read"](map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(path, []byte("alpha beta gamma\n"), 0644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)

	if changed := freshness.Changed(); len(changed) != 1 {
		t.Fatalf("change notice = %v", changed)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("duplicate notice = %v", changed)
	}
	_, err := byName["edit"](map[string]any{"file_path": path, "old_string": "alpha", "new_string": "omega"})
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("stale edit err = %v, want changed-on-disk rejection", err)
	}

	if _, err := byName["read"](map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["edit"](map[string]any{"file_path": path, "old_string": "alpha", "new_string": "omega"}); err != nil {
		t.Fatalf("edit after re-read err = %v", err)
	}
}

func TestBackgroundFreshnessIsIndependentAndSurvivesNotification(t *testing.T) {
	root, dir, cleanup := createTestRoot(t)
	defer cleanup()
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	freshness := fs.NewFreshness(root)
	tools := fs.Tools(root, &fs.Options{Freshness: freshness})
	run := func(ctx context.Context, name string, args map[string]any) error {
		for _, tl := range tools {
			if tl.Name == name {
				_, err := tl.Execute(ctx, args)
				return err
			}
		}
		t.Fatalf("missing %s", name)
		return nil
	}
	first := tool.WithBackgroundOrigin(t.Context(), "first")
	second := tool.WithBackgroundOrigin(t.Context(), "second")
	read := map[string]any{"file_path": path}
	for _, ctx := range []context.Context{t.Context(), first, second} {
		if err := run(ctx, "read", read); err != nil {
			t.Fatal(err)
		}
	}
	edit := map[string]any{"file_path": path, "old_string": "original", "new_string": "changed by first"}
	if err := run(first, "edit", edit); err != nil {
		t.Fatal(err)
	}
	if got := freshness.Changed(); len(got) != 1 {
		t.Fatalf("background write not announced: %v", got)
	}
	edit = map[string]any{"file_path": path, "old_string": "changed by first", "new_string": "another change"}
	for _, ctx := range []context.Context{t.Context(), second} {
		if err := run(ctx, "edit", edit); err == nil || !strings.Contains(err.Error(), "changed on disk") {
			t.Fatalf("stale agent edit = %v", err)
		}
	}
	if err := run(tool.WithBackgroundOrigin(t.Context(), "first"), "edit", edit); err != nil {
		t.Fatalf("first agent lost its baseline: %v", err)
	}
	if err := run(second, "read", read); err != nil {
		t.Fatal(err)
	}
	if err := run(second, "edit", map[string]any{"file_path": path, "old_string": "another change", "new_string": "after reread"}); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessAnnouncesRecreationAndKeepsStaleGuard(t *testing.T) {
	root, dir, cleanup := createTestRoot(t)
	defer cleanup()
	path := filepath.Join(dir, "recreated.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	f := fs.NewFreshness(root)
	tools := fs.Tools(root, &fs.Options{Freshness: f})
	for _, tl := range tools {
		if tl.Name == "read" {
			if _, err := tl.Execute(t.Context(), map[string]any{"file_path": path}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := f.Changed(); len(got) != 1 || !strings.Contains(got[0], "deleted") {
		t.Fatalf("deletion: %v", got)
	}
	if err := os.WriteFile(path, []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.Changed(); len(got) != 1 || got[0] != path {
		t.Fatalf("recreation: %v", got)
	}
	for _, tl := range tools {
		if tl.Name == "edit" {
			_, err := tl.Execute(t.Context(), map[string]any{"file_path": path, "old_string": "new content", "new_string": "bad"})
			if err == nil {
				t.Fatal("recreation cleared unread change")
			}
		}
	}
}

// A session reads without read roots and edits memory through a write root; both must track the same file.
func TestFreshnessTracksOneFileAcrossUnconfinedReadAndWriteRootEdit(t *testing.T) {
	root, _, cleanup := createTestRoot(t)
	defer cleanup()

	// On macOS the temporary directory is spelled through the /var alias of /private/var.
	memory := t.TempDir()
	path := filepath.Join(memory, "MEMORY.md")
	if err := os.WriteFile(path, []byte("alpha beta\n"), 0644); err != nil {
		t.Fatal(err)
	}

	freshness := fs.NewFreshness(root)
	byName := map[string]func(map[string]any) (string, error){}
	for _, tl := range fs.Tools(root, &fs.Options{AllowedWriteRoots: []string{memory}, Freshness: freshness}) {
		byName[tl.Name] = func(args map[string]any) (string, error) {
			result, err := tl.Execute(t.Context(), args)
			return result.Content.Text(), err
		}
	}

	if _, err := byName["read"](map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["edit"](map[string]any{"file_path": path, "old_string": "alpha", "new_string": "omega"}); err != nil {
		t.Fatal(err)
	}
	if changed := freshness.Changed(); len(changed) != 0 {
		t.Fatalf("own edit announced as an external change: %v", changed)
	}

	if err := os.WriteFile(path, []byte("edited externally\n"), 0644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)
	_, err := byName["edit"](map[string]any{"file_path": path, "old_string": "edited", "new_string": "changed"})
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("stale edit after an unconfined read err = %v, want changed-on-disk rejection", err)
	}
}

// Only the shell can change a file outside every root; announcing that as an external change cost a round trip per edit.
func TestFreshnessIgnoresFilesOutsideEveryRoot(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	inside, outside := filepath.Join(tmpDir, "client.go"), filepath.Join(t.TempDir(), "policy.go")
	freshness := fs.NewFreshness(root)
	var read tool.Tool
	for _, tl := range fs.Tools(root, &fs.Options{Freshness: freshness}) {
		if tl.Name == "read" {
			read = tl
		}
	}
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("attempts = 3\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := read.Execute(t.Context(), map[string]any{"file_path": path}); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(2 * time.Second)
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("attempts = 4\n"), 0644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, future, future)
	}
	if changed := freshness.Changed(); len(changed) != 1 || changed[0] != inside {
		t.Fatalf("changed = %v, want only the workspace file %s", changed, inside)
	}
}
