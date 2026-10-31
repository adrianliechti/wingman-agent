package filewatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Bug: one unreadable subdirectory fails every walk, so the scanner never records a baseline and reports no change anywhere in the tree; it should skip the unreadable directory.
func TestReviewWorkspaceResourceScannerSurvivesUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for the current user")
	}
	ctx := context.Background()
	root := t.TempDir()
	start := time.Now().Add(-time.Hour)
	write(t, root, "a.go", "a", start)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	scanner := NewScanner(os.DirFS(root), ".", Options{Recursive: true})
	if _, err := scanner.Changes(ctx); err != nil {
		t.Fatalf("baseline failed because of one unreadable directory: %v", err)
	}
	write(t, root, "a.go", "changed", start.Add(time.Minute))
	changes, err := scanner.Changes(ctx)
	if err != nil || len(changes) != 1 || changes[0] != (Change{Path: "a.go", Type: Changed}) {
		t.Fatalf("changes = %v, %v", changes, err)
	}
}

func TestReviewWorkspaceResourceScannerConcurrentCallersSeeEveryChangeOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	start := time.Now().Add(-time.Hour)
	scanner := NewScanner(os.DirFS(root), ".", Options{Recursive: true, Track: func(_ string, entry os.DirEntry) bool { return !entry.IsDir() }})
	if _, err := scanner.Changes(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	created := map[string]int{}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				changes, err := scanner.Changes(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, change := range changes {
					if change.Type == Created {
						created[change.Path]++
					}
				}
				mu.Unlock()
				select {
				case <-done:
					return
				default:
				}
			}
		})
	}
	for i := range 200 {
		write(t, root, fmt.Sprintf("d%d/f%d.txt", i%7, i), "x", start)
	}
	close(done)
	wg.Wait()
	changes, err := scanner.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Type == Created {
			created[change.Path]++
		}
	}
	if len(created) != 200 {
		t.Fatalf("saw %d created files, want 200", len(created))
	}
	for path, count := range created {
		if count != 1 {
			t.Fatalf("%s reported created %d times", path, count)
		}
	}
}
