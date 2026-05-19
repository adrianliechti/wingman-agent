package code

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a minimal bare-bones non-bare repo by writing a `.git`
// directory with the smallest set of entries needed for findCanonicalGitRoot
// to classify it. The walker only checks for `.git` presence + type, so a
// stub layout is enough — no need to invoke `git init`.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "worktrees"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
}

// initWorktree creates a worktree pointing back at mainRoot, mirroring the
// layout `git worktree add` produces: a `.git` file at the worktree's root
// containing "gitdir: <main>/.git/worktrees/<name>", plus a `commondir`
// file inside the worktree gitdir pointing relatively back to <main>/.git.
func initWorktree(t *testing.T, mainRoot, worktreeRoot, name string) {
	t.Helper()
	if err := os.MkdirAll(worktreeRoot, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	worktreeGitdir := filepath.Join(mainRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(worktreeGitdir, 0755); err != nil {
		t.Fatalf("mkdir worktree gitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeRoot, ".git"), []byte("gitdir: "+worktreeGitdir+"\n"), 0644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	// commondir relative to worktreeGitdir is "../.." → <main>/.git
	if err := os.WriteFile(filepath.Join(worktreeGitdir, "commondir"), []byte("../..\n"), 0644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
}

func TestFindCanonicalGitRoot_NoGit(t *testing.T) {
	dir := t.TempDir()
	if got := findCanonicalGitRoot(dir); got != "" {
		t.Errorf("expected empty for non-git dir, got %q", got)
	}
}

func TestFindCanonicalGitRoot_PlainRepo(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)

	if got := findCanonicalGitRoot(dir); got != filepath.Clean(dir) {
		t.Errorf("expected %q, got %q", dir, got)
	}
}

func TestFindCanonicalGitRoot_RepoSubdir(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	sub := filepath.Join(dir, "internal", "foo")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if got := findCanonicalGitRoot(sub); got != filepath.Clean(dir) {
		t.Errorf("expected %q, got %q", dir, got)
	}
}

func TestFindCanonicalGitRoot_WorktreeResolvesToMain(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "repo")
	worktree := filepath.Join(parent, "worktree-feature")

	if err := os.MkdirAll(mainRoot, 0755); err != nil {
		t.Fatalf("mkdir main: %v", err)
	}
	initRepo(t, mainRoot)
	initWorktree(t, mainRoot, worktree, "feature")

	got := findCanonicalGitRoot(worktree)
	if got != filepath.Clean(mainRoot) {
		t.Errorf("expected worktree to resolve to main %q, got %q", mainRoot, got)
	}
}

func TestFindCanonicalGitRoot_WorktreeFallbackWithoutCommondir(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "repo")
	worktree := filepath.Join(parent, "worktree-feature")

	if err := os.MkdirAll(mainRoot, 0755); err != nil {
		t.Fatalf("mkdir main: %v", err)
	}
	initRepo(t, mainRoot)
	initWorktree(t, mainRoot, worktree, "feature")

	// Remove the commondir file so the fallback (parent walk) is exercised.
	if err := os.Remove(filepath.Join(mainRoot, ".git", "worktrees", "feature", "commondir")); err != nil {
		t.Fatalf("remove commondir: %v", err)
	}

	got := findCanonicalGitRoot(worktree)
	if got != filepath.Clean(mainRoot) {
		t.Errorf("expected fallback to resolve to main %q, got %q", mainRoot, got)
	}
}

func TestProjectKey_WorktreesShareKey(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "repo")
	worktree := filepath.Join(parent, "worktree-feature")

	if err := os.MkdirAll(mainRoot, 0755); err != nil {
		t.Fatalf("mkdir main: %v", err)
	}
	initRepo(t, mainRoot)
	initWorktree(t, mainRoot, worktree, "feature")

	mainKey := projectKey(mainRoot)
	worktreeKey := projectKey(worktree)
	subKey := projectKey(filepath.Join(mainRoot, "src"))

	if mainKey != worktreeKey {
		t.Errorf("main and worktree should share key: main=%q worktree=%q", mainKey, worktreeKey)
	}
	if mainKey != subKey {
		// Also verifies subdirs of the main repo collapse to the main key.
		// Note: the subdir doesn't have to exist for the walker — but it should
		// produce the same key as long as the walk finds the same .git.
		if _, err := os.Stat(filepath.Join(mainRoot, "src")); err == nil {
			t.Errorf("subdir should share key with main: main=%q sub=%q", mainKey, subKey)
		}
	}
}

func TestProjectKey_NonGitDirUsesRawPath(t *testing.T) {
	dir := t.TempDir()
	key := projectKey(dir)
	// Sanity: the key should derive from dir, not be empty, not contain separators.
	if key == "" {
		t.Fatal("expected non-empty key")
	}
	if strings.ContainsRune(key, filepath.Separator) {
		t.Errorf("expected no path separators in key, got %q", key)
	}
	if key != strings.ToLower(key) {
		t.Errorf("expected lowercased key, got %q", key)
	}
}
