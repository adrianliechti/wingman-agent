package fs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestTransactionRejectsChangedPreconditionsWithoutPartialWrites(t *testing.T) {
	for _, scenario := range []string{"modified", "deleted", "created", "permissions"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			for _, name := range []string{"first", "second"} {
				if err := root.WriteFile(name, []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tx := &fileTransaction{}
			for _, name := range []string{"first", "second"} {
				target, err := resolveFileTarget(name, dir, nil, "edit")
				if err != nil {
					t.Fatal(err)
				}
				tx.changes = append(tx.changes, fileChange{target: target, displayPath: name, before: []byte("before"), beforePresent: true, beforeMode: 0600, after: []byte("after"), afterPresent: true})
			}
			switch scenario {
			case "modified":
				err = root.WriteFile("second", []byte("external edit"), 0600)
			case "deleted":
				err = root.Remove("second")
			case "created":
				tx.changes[1].beforePresent = false
				tx.changes[1].before = nil
			case "permissions":
				err = root.Chmod("second", 0400)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := commitFileTransaction(t.Context(), root, tx); err == nil || !strings.Contains(err.Error(), "changed on disk") {
				t.Fatalf("commit error: %v", err)
			}
			first, err := root.ReadFile("first")
			if err != nil || string(first) != "before" {
				t.Fatalf("partial write: %q, %v", first, err)
			}
			debris, err := filepath.Glob(filepath.Join(dir, ".wingman-*"))
			if err != nil || len(debris) != 0 {
				t.Fatalf("transaction debris: %v, %v", debris, err)
			}
		})
	}
}

func TestConcurrentPreparedTransactionsCannotOverwriteEachOther(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("shared", []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := resolveFileTarget("shared", dir, nil, "edit")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, after := range []string{"one", "two"} {
		wg.Go(func() {
			tx := &fileTransaction{changes: []fileChange{{target: target, displayPath: "shared", before: []byte("before"), beforePresent: true, beforeMode: 0600, after: []byte(after), afterPresent: true}}}
			results <- commitFileTransaction(t.Context(), root, tx)
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "changed on disk") {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful conflicting transactions = %d", successes)
	}
}
