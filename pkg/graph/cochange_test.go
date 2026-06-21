package graph

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestCoChanges(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	commit := func(msg string, files ...string) {
		t.Helper()
		for _, f := range files {
			if _, err := wt.Add(f); err != nil {
				t.Fatal(err)
			}
		}
		_, err := wt.Commit(msg, &git.CommitOptions{
			Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, dir, "a.go", "package p\n")
	writeFile(t, dir, "b.go", "package p\n")
	commit("c1", "a.go", "b.go")

	writeFile(t, dir, "a.go", "package p\n// v2\n")
	writeFile(t, dir, "c.go", "package p\n")
	commit("c2", "a.go", "c.go")

	writeFile(t, dir, "a.go", "package p\n// v3\n")
	commit("c3", "a.go")

	e := New(dir, filepath.Join(t.TempDir(), "graph.json"))
	res, err := e.CoChanges(context.Background(), "a.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commits != 3 {
		t.Fatalf("commits touching a.go = %d, want 3", res.Commits)
	}

	got := map[string]int{}
	for _, c := range res.Related {
		got[c.File] = c.Count
	}
	if got["b.go"] != 1 || got["c.go"] != 1 || len(got) != 2 {
		t.Fatalf("co-changes = %+v, want b.go:1 c.go:1", got)
	}
}
