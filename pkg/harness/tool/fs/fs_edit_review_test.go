package fs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestEditRejectsOverlappingMatches(t *testing.T) {
	for _, tc := range []struct{ name, content, old string }{
		{"exact", "ababa", "aba"},
		{"fuzzy", "a— a— a", "a- a"},
		{"indentation", "\ta\n\ta\n\ta", "a\na"},
		{"blank lines", "a\n\na\n\na", "a\na"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applyEditOp(tc.content, editOp{oldText: tc.old, newText: "replacement"}, "file")
			if err == nil || !strings.Contains(err.Error(), "2 occurrences") {
				t.Fatalf("ambiguous edit error = %v", err)
			}
		})
	}
}

func TestEditPreservesQuotesWhenWhitespaceAlsoDiffers(t *testing.T) {
	got, err := applyEditOp("say “hello”   \nnext", editOp{oldText: "say \"hello\"\nnext", newText: "say \"bye\"\nnext"}, "file")
	if err != nil || got != "say “bye”\nnext" {
		t.Fatalf("edit = %q, %v", got, err)
	}
}

func TestEditRejectsUnsafeTextWithoutPartialWrites(t *testing.T) {
	for _, tc := range []struct{ name, before, old, replacement, wantErr string }{
		{"binary source", "hello\x00\r\n", "hello", "bye", "UTF-8 text"},
		{"invalid UTF-8 source", "hello\xff\n", "hello", "bye", "UTF-8 text"},
		{"binary replacement", "hello\n", "hello", "bye\x00", "UTF-8 text"},
		{"whitespace is not empty", " \t\n", "", "replacement", "already has content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("file", []byte(tc.before), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{
				map[string]any{"file_path": "created", "old_string": "", "new_string": "must not exist"},
				map[string]any{"file_path": "file", "old_string": tc.old, "new_string": tc.replacement},
			}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v", err)
			}
			got, readErr := root.ReadFile("file")
			if readErr != nil || string(got) != tc.before {
				t.Fatalf("file changed: %q, %v", got, readErr)
			}
			if _, statErr := root.Stat("created"); !os.IsNotExist(statErr) {
				t.Fatalf("partial creation: %v", statErr)
			}
		})
	}
}

func TestEditCapsExpandedResults(t *testing.T) {
	replacement := strings.Repeat("x", MaxEditFileBytes/2+1)
	for _, tc := range []struct {
		name, content, old string
		all                bool
	}{
		{"creation", "", "", false},
		{"single", "ab", "a", false},
		{"exact replace all", "aa", "a", true},
		{"fuzzy replace all", "a—a—", "a-", true},
		{"indent replace all", "\ta\n\tb\n\ta\n\tb", "a\nb", true},
		{"blank line replace all", "a\n\nb\na\n\nb", "a\nb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newText := replacement
			if !tc.all {
				newText = strings.Repeat("x", MaxEditFileBytes+1)
				if tc.name == "single" {
					newText = newText[:MaxEditFileBytes]
				}
			}
			_, err := applyEditOp(tc.content, editOp{oldText: tc.old, newText: newText, replaceAll: tc.all}, "file")
			if err == nil || !strings.Contains(err.Error(), "capped at") {
				t.Fatalf("size error = %v", err)
			}
		})
	}
}

func TestEditCapsSerializedLineEndingsBeforeCommit(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := []byte("\uFEFFa\r\n")
	if err := root.WriteFile("file", before, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{
		map[string]any{"file_path": "created", "old_string": "", "new_string": "must not exist"},
		map[string]any{"file_path": "file", "old_string": "a", "new_string": strings.Repeat("\n", MaxEditFileBytes/2)},
	}})
	if err == nil || !strings.Contains(err.Error(), "capped at") {
		t.Fatalf("size error = %v", err)
	}
	got, err := root.ReadFile("file")
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("file changed: %q, %v", got, err)
	}
	if _, err := root.Stat("created"); !os.IsNotExist(err) {
		t.Fatalf("partial creation: %v", err)
	}
}

func TestEditRejectsNonBooleanReplaceAll(t *testing.T) {
	_, err := newEditOp(map[string]any{"old_string": "a", "new_string": "b", "replace_all": "true"})
	if err == nil || !strings.Contains(err.Error(), "boolean") {
		t.Fatalf("error = %v", err)
	}
}

func TestEditMetadataSurvivesPersistenceAndUndo(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := []byte("\uFEFFsay “hello”  \r\nnext\r\n")
	if err := root.WriteFile("file", before, 0750); err != nil {
		t.Fatal(err)
	}
	// Windows only represents the read-only attribute; expect the reported mode.
	original, err := root.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	result, err := EditTool(root).Execute(t.Context(), map[string]any{"edits": []any{
		map[string]any{"file_path": "created", "old_string": "", "new_string": "created\n"},
		map[string]any{"file_path": "file", "old_string": "say \"hello\"\nnext", "new_string": "say \"bye\"\nnext"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	changes, err := tool.ResultFileChanges(metadata)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %v, %v", changes, err)
	}
	for _, change := range changes {
		after, err := root.ReadFile(change.Path)
		if err != nil || string(after) != change.After {
			t.Fatalf("incorrect after state for %s", change.Path)
		}
		if change.Path == "file" && (change.Before != string(before) || change.Mode != uint32(original.Mode().Perm())) {
			t.Fatalf("incorrect baseline: %+v", change)
		}
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	got, err := root.ReadFile("file")
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("restored file = %q, %v", got, err)
	}
	if _, err := root.Stat("created"); !os.IsNotExist(err) {
		t.Fatalf("created file retained: %v", err)
	}
}

func TestEditMetadataUsesAbsolutePathsOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	workspace, allowed := filepath.Join(dir, "workspace"), filepath.Join(dir, "allowed")
	for _, path := range []string{workspace, allowed} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	result, err := EditTool(root, allowed).Execute(t.Context(), map[string]any{
		"file_path": "../allowed/file", "old_string": "", "new_string": "created",
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := tool.ResultFileChanges(result.Metadata)
	if err != nil || len(changes) != 1 || !filepath.IsAbs(changes[0].Path) || resolveForCompare(changes[0].Path) != resolveForCompare(filepath.Join(allowed, "file")) {
		t.Fatalf("external file changes = %v, %v", changes, err)
	}
}

func TestEditFuzzyReplaceAllAcrossWhitespaceOnlyLines(t *testing.T) {
	got, err := applyEditOp("a\n   \nb\n   \n", editOp{oldText: "\n\t", newText: "!", replaceAll: true}, "file")
	if err != nil || got != "a!!b!!" {
		t.Fatalf("replacement = %q, %v", got, err)
	}
}

func TestEditPreservesCarriageReturnLineEndings(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("file", []byte("first\rsecond\r"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = EditTool(root).Execute(t.Context(), map[string]any{"file_path": "file", "old_string": "second", "new_string": "last"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := root.ReadFile("file")
	if err != nil || !bytes.Equal(got, []byte("first\rlast\r")) {
		t.Fatalf("content = %q, %v", got, err)
	}
}
