package filewatch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, name, content string, modified time.Time) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestScannerReportsChangesSinceTheLastScan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	start := time.Now().Add(-time.Hour)
	write(t, root, "a.go", "a", start)
	write(t, root, "b.go", "b", start)
	write(t, root, "node_modules/x.js", "x", start)
	skip := func(rel string, entry fs.DirEntry) bool { return entry.IsDir() && rel == "node_modules" }
	track := func(_ string, entry fs.DirEntry) bool { return !entry.IsDir() }
	scanner := NewScanner(os.DirFS(root), ".", Options{Recursive: true, Skip: skip, Track: track})
	if changes, err := scanner.Changes(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("baseline reported %v, %v", changes, err)
	}
	write(t, root, "a.go", "changed", start.Add(time.Minute))
	write(t, root, "c/d.go", "new", start)
	write(t, root, "node_modules/x.js", "skipped", start.Add(time.Minute))
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	changes, err := scanner.Changes(ctx)
	want := []Change{{Path: "a.go", Type: Changed}, {Path: "b.go", Type: Deleted}, {Path: "c/d.go", Type: Created}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v, %v; want %v", changes, err, want)
	}
	if changes, err := scanner.Changes(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("unchanged tree reported %v, %v", changes, err)
	}
}

func TestScannerScopesToItsRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	start := time.Now().Add(-time.Hour)
	write(t, root, "src/top.go", "a", start)
	write(t, root, "src/nested/deep.go", "a", start)
	scanner := NewScanner(os.DirFS(root), "src", Options{})
	if _, err := scanner.Changes(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/top.go", "b", start.Add(time.Minute))
	write(t, root, "src/nested/deep.go", "b", start.Add(time.Minute))
	changes, err := scanner.Changes(ctx)
	if err != nil || len(changes) != 1 || changes[0] != (Change{Path: "src/top.go", Type: Changed}) {
		t.Fatalf("non-recursive changes = %v, %v", changes, err)
	}
}

func TestScannerLimitKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	start := time.Now().Add(-time.Hour)
	write(t, root, "a.go", "a", start)
	scanner := NewScanner(os.DirFS(root), ".", Options{Recursive: true, Limit: 3, Track: func(_ string, entry fs.DirEntry) bool { return !entry.IsDir() }})
	if _, err := scanner.Changes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.go", "c.go"} {
		write(t, root, name, "x", start)
	}
	if _, err := scanner.Changes(ctx); !errors.Is(err, ErrLimit) {
		t.Fatalf("err = %v, want ErrLimit", err)
	}
	if err := os.Remove(filepath.Join(root, "c.go")); err != nil {
		t.Fatal(err)
	}
	changes, err := scanner.Changes(ctx)
	if err != nil || len(changes) != 1 || !strings.HasSuffix(changes[0].Path, "b.go") || changes[0].Type != Created {
		t.Fatalf("after a failed walk: %v, %v", changes, err)
	}
}
