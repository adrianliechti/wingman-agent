package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type cancelWhenInstalledContext struct {
	context.Context
	cancel context.CancelFunc
	root   *os.Root
}

func (c cancelWhenInstalledContext) Err() error {
	if current, _ := c.root.ReadFile("first"); string(current) == "after" {
		c.cancel()
	}
	return c.Context.Err()
}

type cancelWhenBackedUpContext struct {
	context.Context
	cancel context.CancelFunc
	dir    string
}

func (c cancelWhenBackedUpContext) Err() error {
	if matches, _ := filepath.Glob(filepath.Join(c.dir, ".wingman-backup-*")); len(matches) > 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestTransactionRollsBackAndPreservesContextErrorDuringBackupCheck(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("first", []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := resolveFileTarget("first", dir, nil, "edit")
	if err != nil {
		t.Fatal(err)
	}
	tx := &fileTransaction{changes: []fileChange{{
		target:        target,
		displayPath:   "first",
		before:        []byte("before"),
		beforePresent: true,
		beforeMode:    0600,
		after:         []byte("after"),
		afterPresent:  true,
	}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fileEditMu.Lock()
	err = commitFileTransactionLocked(cancelWhenBackedUpContext{ctx, cancel, dir}, root, tx)
	fileEditMu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", err)
	}
	if current, err := root.ReadFile("first"); err != nil || string(current) != "before" {
		t.Fatalf("rollback = %q, %v", current, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "first" {
		t.Fatalf("transaction debris = %v, %v", entries, err)
	}
}

func TestTransactionRollsBackAfterCancellationDuringCommit(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("first", []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	tx := &fileTransaction{}
	for _, name := range []string{"first", "second"} {
		target, err := resolveFileTarget(name, dir, nil, "edit")
		if err != nil {
			t.Fatal(err)
		}
		change := fileChange{target: target, displayPath: name, beforeMode: 0600, after: []byte("after"), afterPresent: true}
		if name == "first" {
			change.before, change.beforePresent = []byte("before"), true
		}
		tx.changes = append(tx.changes, change)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fileEditMu.Lock()
	err = commitFileTransactionLocked(cancelWhenInstalledContext{ctx, cancel, root}, root, tx)
	fileEditMu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v", err)
	}
	if current, err := root.ReadFile("first"); err != nil || string(current) != "before" {
		t.Fatalf("rollback = %q, %v", current, err)
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil || len(entries) != 1 || entries[0].Name() != "first" {
		t.Fatalf("transaction debris = %v, %v", entries, err)
	}
}

func TestTransactionContentReadIsBounded(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("file", []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int{0, 5} {
		got, err := readTransactionContent(t.Context(), root, "file", expected)
		if err != nil || len(got) != expected+1 {
			t.Fatalf("bounded read = %d bytes, %v", len(got), err)
		}
	}
}

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
			fileEditMu.Lock()
			err = commitFileTransactionLocked(t.Context(), root, tx)
			fileEditMu.Unlock()
			if err == nil || !strings.Contains(err.Error(), "changed on disk") {
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
			fileEditMu.Lock()
			err := commitFileTransactionLocked(t.Context(), root, tx)
			fileEditMu.Unlock()
			results <- err
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
