package changes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewWorkspaceChangesetSymlinkedVendor(t *testing.T) (dir, outside string) {
	t.Helper()
	dir = t.TempDir()
	repo := initRepository(t, dir)
	writeFile(t, dir, "vendor/lib.txt", "tracked\n")
	stage(t, repo, "vendor/lib.txt")
	commit(t, repo, "initial")
	outside = t.TempDir()
	writeFile(t, outside, "lib.txt", "outside\n")
	if err := os.RemoveAll(filepath.Join(dir, "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "vendor")); err != nil {
		t.Fatal(err)
	}
	return dir, outside
}

func reviewWorkspaceChangesetRequireDeleted(t *testing.T, m *Manager, path string) {
	t.Helper()
	unstaged, err := m.DiffsLayer(context.Background(), DiffUnstaged)
	if err != nil {
		t.Fatal(err)
	}
	for _, diff := range unstaged {
		if diff.Path == path && diff.Status == StatusDeleted {
			return
		}
	}
	t.Fatalf("%s is not listed as an unstaged deletion: %+v", path, unstaged)
}

// Bug: Revert restores an index entry through a symlinked parent directory, so discarding a deleted file overwrites a file outside the workspace; it must never follow a symlink in the restored path.
func TestReviewWorkspaceChangesetRevertDoesNotWriteThroughSymlinkedParent(t *testing.T) {
	dir, outside := reviewWorkspaceChangesetSymlinkedVendor(t)
	m := New(dir)
	defer m.Close()
	reviewWorkspaceChangesetRequireDeleted(t, m, "vendor/lib.txt")
	_ = m.Revert(context.Background(), "vendor/lib.txt")
	if got := readFile(t, outside, "lib.txt"); got != "outside\n" {
		t.Fatalf("discard wrote outside the workspace: %q", got)
	}
}

func TestReviewWorkspaceChangesetStageDoesNotReadThroughSymlinkedParent(t *testing.T) {
	dir, _ := reviewWorkspaceChangesetSymlinkedVendor(t)
	m := New(dir)
	defer m.Close()
	reviewWorkspaceChangesetRequireDeleted(t, m, "vendor/lib.txt")
	_ = m.Stage(context.Background(), []string{"vendor/lib.txt"})
	data, _, exists, err := m.indexFile("vendor/lib.txt")
	if err != nil {
		t.Fatal(err)
	}
	if exists && string(data) == "outside\n" {
		t.Fatal("index now holds the content of a file outside the workspace")
	}
}

func TestReviewWorkspaceChangesetBinaryPatchIdentifiesContent(t *testing.T) {
	dir := t.TempDir()
	repo := initRepository(t, dir)
	writeFile(t, dir, "image.bin", "\x00base")
	stage(t, repo, "image.bin")
	commit(t, repo, "initial")
	m := New(dir)
	defer m.Close()
	patches := map[string]bool{}
	for _, content := range []string{"\x00first", "\x00second"} {
		writeFile(t, dir, "image.bin", content)
		diffs, err := m.DiffsLayer(context.Background(), DiffCombined)
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 1 || !diffs[0].Binary || diffs[0].Original != "" || diffs[0].Modified != "" {
			t.Fatalf("binary diff = %+v", diffs)
		}
		patches[diffs[0].Patch] = true
	}
	// Changeset nonces hash the patch; identical binary patches would let a stale discard through.
	if len(patches) != 2 {
		t.Fatal("different binary contents produce the same patch")
	}
}

func TestReviewWorkspaceChangesetCompareReportsRenameAndBinary(t *testing.T) {
	dir := t.TempDir()
	repo := initRepository(t, dir)
	body := strings.Repeat("line\n", 20)
	writeFile(t, dir, "old.txt", body)
	writeFile(t, dir, "blob.bin", "\x00one")
	stage(t, repo, "old.txt")
	stage(t, repo, "blob.bin")
	commit(t, repo, "initial")
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "blob.bin", "\x00two")
	stage(t, repo, "old.txt")
	stage(t, repo, "new.txt")
	stage(t, repo, "blob.bin")
	commit(t, repo, "rename")
	m := New(dir)
	defer m.Close()
	result, err := m.Compare(context.Background(), "HEAD~1", "HEAD", false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, diff := range result.Diffs {
		byPath[diff.Path] = diff
	}
	renamed, ok := byPath["new.txt"]
	if len(byPath) != 2 || !ok || renamed.OriginalPath != "old.txt" || renamed.Status != StatusModified || renamed.Original != body || renamed.Modified != body {
		t.Fatalf("diffs = %+v", result.Diffs)
	}
	if blob := byPath["blob.bin"]; !blob.Binary || blob.Original != "" || blob.Modified != "" || blob.Status != StatusModified {
		t.Fatalf("binary diff = %+v", blob)
	}
}
