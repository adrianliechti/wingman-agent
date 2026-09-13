package fs

import (
	"context"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"os"
	"path/filepath"
	"testing"
)

func TestUndoPreservesPreexistingUserEditsAndRejectsLaterChanges(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve dirty baseline", true: "refuse newer edits"}[later], func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			after := "user's uncommitted work + agent changes"
			current := after
			if later {
				current += " + user changes"
			}
			if err := os.WriteFile(filepath.Join(dir, "file"), []byte(current), 0644); err != nil {
				t.Fatal(err)
			}
			changes := []tool.FileChange{{Path: "file", Before: "user's uncommitted work", After: after, BeforeExists: true, AfterExists: true, Mode: 0644}, {Path: "new", After: "new file", AfterExists: true, Mode: 0644}}
			if err := os.WriteFile(filepath.Join(dir, "new"), []byte("new file"), 0644); err != nil {
				t.Fatal(err)
			}
			err = UndoFileChanges(context.Background(), root, changes)
			data, _ := root.ReadFile("file")
			if later {
				if err == nil || string(data) != current {
					t.Fatalf("overwrote newer work: %q %v", data, err)
				}
				if _, err := root.Stat("new"); err != nil {
					t.Fatal("partially undid batch")
				}
			} else {
				if err != nil || string(data) != "user's uncommitted work" {
					t.Fatalf("undo: %q %v", data, err)
				}
				if _, err := root.Stat("new"); !os.IsNotExist(err) {
					t.Fatal("created file retained")
				}
			}
		})
	}
}
func TestUndoRejectsDiscontinuousEditsAndEscapingPaths(t *testing.T) {
	dir := t.TempDir()
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	os.WriteFile(filepath.Join(dir, "file"), []byte("third"), 0644)
	changes := []tool.FileChange{{Path: "file", Before: "first", After: "second", BeforeExists: true, AfterExists: true, Mode: 0644}, {Path: "file", Before: "external", After: "third", BeforeExists: true, AfterExists: true, Mode: 0644}}
	if UndoFileChanges(t.Context(), root, changes) == nil {
		t.Fatal("undid across external edits")
	}
	for _, path := range []string{"../escape", "/absolute"} {
		if UndoFileChanges(t.Context(), root, []tool.FileChange{{Path: path}}) == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}
