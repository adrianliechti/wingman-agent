package fs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestNativeMoveAndUndo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents []byte
	}{
		{"large binary", bytes.Repeat([]byte{0xff, 0}, MaxEditFileBytes/2+1)},
		{"large text", bytes.Repeat([]byte("Z"), MaxEditFileBytes+1)},
		{"small binary", []byte{0xff, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("original", tc.contents, 0750); err != nil {
				t.Fatal(err)
			}
			original, err := root.Stat("original")
			if err != nil {
				t.Fatal(err)
			}
			edit := EditTool(root)
			result, err := edit.Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": "original", "move_to": "nested/new", "old_string": "", "new_string": ""}}})
			if err != nil {
				t.Fatal(err)
			}
			moved, err := root.Stat("nested/new")
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(original, moved) || original.Mode() != moved.Mode() {
				t.Fatalf("rename replaced inode or mode: %v -> %v", original, moved)
			}
			if _, err := root.Stat("original"); !os.IsNotExist(err) {
				t.Fatalf("source still present: %v", err)
			}
			changes, err := tool.ResultFileChanges(result.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			ref := &tool.FileContentRef{SHA256: fmt.Sprintf("%x", sha256.Sum256(tc.contents)), Size: int64(len(tc.contents))}
			if len(changes) != 2 || !ref.Equal(changes[0].BeforeRef) || !ref.Equal(changes[1].AfterRef) || changes[1].MovedFrom != "original" {
				t.Fatalf("move metadata = %+v", changes)
			}
			raw, err := json.Marshal(changes)
			if err != nil || len(raw) > 1024 {
				t.Fatalf("journal metadata = %d bytes, %v", len(raw), err)
			}
			var durable []tool.FileChange
			if err := json.Unmarshal(raw, &durable); err != nil {
				t.Fatal(err)
			}
			altered := bytes.Clone(tc.contents)
			altered[0] ^= 1
			if err := root.WriteFile("nested/new", altered, 0750); err != nil {
				t.Fatal(err)
			}
			if err := root.Chtimes("nested/new", moved.ModTime(), moved.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := UndoFileChanges(t.Context(), root, durable); err == nil {
				t.Fatal("undo accepted a same-size content change")
			}
			if _, err := root.Stat("original"); !os.IsNotExist(err) {
				t.Fatal("failed undo recreated the source")
			}
			if err := root.WriteFile("nested/new", tc.contents, 0750); err != nil {
				t.Fatal(err)
			}
			if err := UndoFileChanges(t.Context(), root, durable); err != nil {
				t.Fatal(err)
			}
			after, err := root.ReadFile("original")
			if err != nil || !bytes.Equal(after, tc.contents) {
				t.Fatal("undo lost the original bytes")
			}
			if _, err := root.Stat("nested/new"); !os.IsNotExist(err) {
				t.Fatal("undo left destination")
			}
		})
	}
}

func TestMoveAcrossWriteRootsAndThroughIntermediatePath(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir := t.TempDir()
	external := filepath.Join(dir, "final")
	content := []byte{0xff, 0, 31}
	if err := root.WriteFile("start", content, 0700); err != nil {
		t.Fatal(err)
	}
	var changes []tool.FileChange
	edit := EditTool(root, dir)
	for _, pair := range [][2]string{{"start", "middle"}, {"middle", external}} {
		result, err := edit.Execute(t.Context(), map[string]any{"file_path": pair[0], "move_to": pair[1]})
		if err != nil {
			t.Fatal(err)
		}
		updates, err := tool.ResultFileChanges(result.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, updates...)
	}
	net, gap := tool.NetFileChanges(changes)
	if gap != "" || len(net) != 3 || net[2].MovedFrom != "start" {
		t.Fatalf("net = %+v; gap=%q", net, gap)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err == nil {
		t.Fatal("undo ignored the external write scope")
	}
	if err := root.WriteFile("middle", []byte("new work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UndoFileChanges(t.Context(), root, changes, dir); err != nil {
		t.Fatal(err)
	}
	if data, err := root.ReadFile("start"); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("restored %x, %v", data, err)
	}
	if b, err := root.ReadFile("middle"); err != nil || string(b) != "new work" {
		t.Fatal("undo changed an intermediate path")
	}
	if _, err := os.Stat(external); !os.IsNotExist(err) {
		t.Fatalf("still exists: %v", err)
	}
}

func TestPureTextMovePreservesMixedLineEndings(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	content := []byte("\xef\xbb\xbfA\r\nB\nC\r\n")
	if err := root.WriteFile("src", content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EditTool(root).Execute(t.Context(), map[string]any{"file_path": "src", "move_to": "dst"}); err != nil {
		t.Fatal(err)
	}
	if data, err := root.ReadFile("dst"); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("renamed text = %q, %v", data, err)
	}
}

func TestBinaryMoveValidationIsAtomic(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"src", "occupied"} {
		if err := root.WriteFile(name, []byte{0xff, 0}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	edit := EditTool(root)
	for _, op := range []map[string]any{
		{"file_path": "src", "move_to": "occupied"},
		{"file_path": "src", "move_to": "dst", "old_string": "a", "new_string": "b"},
		{"file_path": "src", "move_to": filepath.Join(t.TempDir(), "outside")},
	} {
		_, err := edit.Execute(t.Context(), map[string]any{"edits": []any{map[string]any{"file_path": "txt", "old_string": "", "new_string": "pending"}, op}})
		if err == nil {
			t.Fatal("invalid batch succeeded")
		}
		if _, err := root.Stat("txt"); !os.IsNotExist(err) {
			t.Fatal("invalid batch wrote a file")
		}
		if data, err := root.ReadFile("src"); err != nil || !bytes.Equal(data, []byte{0xff, 0}) {
			t.Fatal("invalid batch changed source")
		}
	}
}

func TestBinarySwapRetainsBothInodesOnUndo(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"a", "b"} {
		if err := root.WriteFile(name, []byte{0, 0xff}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := root.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.Stat("b")
	if err != nil {
		t.Fatal(err)
	}
	var entries []any
	for _, pair := range [][2]string{{"a", "temp"}, {"b", "a"}, {"temp", "b"}} {
		entries = append(entries, map[string]any{"file_path": pair[0], "move_to": pair[1]})
	}
	result, err := EditTool(root).Execute(t.Context(), map[string]any{"edits": entries})
	if err != nil {
		t.Fatal(err)
	}
	swapped, err := root.Stat("b")
	if err != nil || !os.SameFile(first, swapped) {
		t.Fatal("equal bytes hid a real move")
	}
	records, err := tool.ResultFileChanges(result.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := UndoFileChanges(t.Context(), root, records); err != nil {
		t.Fatal(err)
	}
	restoredA, err := root.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	restoredB, err := root.Stat("b")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, restoredA) || !os.SameFile(second, restoredB) {
		t.Fatal("undo conflated two identical files")
	}
}

func TestUndoLeavesRoundTripRenameAlone(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("start", []byte{0, 1}, 0600); err != nil {
		t.Fatal(err)
	}
	var changes []tool.FileChange
	for _, pair := range [][2]string{{"start", "temp"}, {"temp", "start"}} {
		result, err := EditTool(root).Execute(t.Context(), map[string]any{"file_path": pair[0], "move_to": pair[1]})
		if err != nil {
			t.Fatal(err)
		}
		records, err := tool.ResultFileChanges(result.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, records...)
	}
	if err := root.WriteFile("start", []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UndoFileChanges(t.Context(), root, changes); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile("start")
	if err != nil || string(data) != "new content" {
		t.Fatalf("unexpected restored contents: %q, %v", data, err)
	}
}
