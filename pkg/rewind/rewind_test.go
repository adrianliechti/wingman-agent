package rewind

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "sub/b.txt", "world")
	writeFile(t, dir, "sub/deep/c.txt", "deep")

	m := New(dir)
	defer m.Cleanup()

	fp1 := m.Fingerprint()
	if fp1 == 0 {
		t.Fatal("fingerprint is zero")
	}
	if fp2 := m.Fingerprint(); fp2 != fp1 {
		t.Fatalf("fingerprint not stable: %x vs %x", fp1, fp2)
	}

	writeFile(t, dir, "sub/d.txt", "new")
	fp3 := m.Fingerprint()
	if fp3 == fp1 {
		t.Fatal("fingerprint unchanged after adding a file")
	}

	if err := os.Remove(filepath.Join(dir, "sub", "d.txt")); err != nil {
		t.Fatal(err)
	}
	if fp4 := m.Fingerprint(); fp4 != fp1 {
		t.Fatalf("fingerprint did not revert after removing the file: %x vs %x", fp4, fp1)
	}
}

func TestFingerprintIgnoresGitDirAndGitignored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, ".gitignore", "ignored/\n")

	m := New(dir)
	defer m.Cleanup()

	fp1 := m.Fingerprint()

	writeFile(t, dir, ".git/config", "x")
	writeFile(t, dir, "ignored/file.txt", "x")

	if fp2 := m.Fingerprint(); fp2 != fp1 {
		t.Fatalf("fingerprint changed for .git / gitignored content: %x vs %x", fp1, fp2)
	}
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCommitListRestore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "one")
	writeFile(t, dir, "sub/b.txt", "two")

	m := New(dir)
	defer m.Cleanup()

	cps, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 || cps[0].Message != "Session Start" {
		t.Fatalf("unexpected baseline checkpoints: %+v", cps)
	}
	baseline := cps[0].Hash

	writeFile(t, dir, "a.txt", "changed")
	writeFile(t, dir, "c.txt", "new")
	writeFile(t, dir, "deep/nested/d.txt", "deep")
	if err := os.Remove(filepath.Join(dir, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}

	if err := m.Commit("turn 1"); err != nil {
		t.Fatal(err)
	}

	cps, err = m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 2 || cps[0].Message != "turn 1" {
		t.Fatalf("unexpected checkpoints after commit: %+v", cps)
	}

	if err := m.Commit("noop"); err != nil {
		t.Fatal(err)
	}
	if cps, _ = m.List(); len(cps) != 2 {
		t.Fatalf("clean commit created a checkpoint: %+v", cps)
	}

	if err := m.Restore(baseline); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "a.txt"); got != "one" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readFile(t, dir, "sub/b.txt"); got != "two" {
		t.Fatalf("sub/b.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt still exists after restore")
	}
	if _, err := os.Stat(filepath.Join(dir, "deep")); !os.IsNotExist(err) {
		t.Fatal("deep/ not pruned after restore")
	}

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("unexpected diffs after restore: %+v", diffs)
	}
}

func TestDiffFromBaseline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello\n")
	writeFile(t, dir, "gone.txt", "bye\n")

	m := New(dir)
	defer m.Cleanup()
	if err := m.ready(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, "a.txt", "hello\nworld\n")
	writeFile(t, dir, "b.txt", "new\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if len(byPath) != 3 {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}

	a := byPath["a.txt"]
	if a.Status != StatusModified || a.Original != "hello\n" || a.Modified != "hello\nworld\n" {
		t.Fatalf("a.txt diff: %+v", a)
	}
	if !strings.Contains(a.Patch, "+world") {
		t.Fatalf("a.txt patch: %q", a.Patch)
	}
	if byPath["b.txt"].Status != StatusAdded {
		t.Fatalf("b.txt diff: %+v", byPath["b.txt"])
	}
	if byPath["gone.txt"].Status != StatusDeleted {
		t.Fatalf("gone.txt diff: %+v", byPath["gone.txt"])
	}
}

func TestGitRepoBaseline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tracked.txt", "v1")

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("tracked.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@local", When: time.Now()}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, "untracked.txt", "u")

	m := New(dir)
	defer m.Cleanup()

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "untracked.txt" || diffs[0].Status != StatusAdded {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}

	writeFile(t, dir, "tracked.txt", "v2")

	diffs, err = m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}

	cps, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(cps[len(cps)-1].Hash); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "tracked.txt"); got != "v1" {
		t.Fatalf("tracked.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatal("untracked.txt still exists after restore")
	}
}

func TestAutoCRLFNoPhantomDiff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tracked.txt", "line1\nline2\n")

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("tracked.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@local", When: time.Now()}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}

	// Enable autocrlf, then rewrite the working tree as CRLF the way git's
	// checkout would on Windows. The committed blob stays LF.
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.Section("core").SetOption("autocrlf", "true")
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "tracked.txt", "line1\r\nline2\r\n")

	m := New(dir)
	defer m.Cleanup()

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("CRLF-only working tree produced phantom diffs: %+v", diffs)
	}

	// A genuine edit still shows up, with a clean LF patch (no \r noise).
	writeFile(t, dir, "tracked.txt", "line1\r\nCHANGED\r\n")
	diffs, err = m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Status != StatusModified {
		t.Fatalf("expected one modified diff, got: %+v", diffs)
	}
	if strings.Contains(diffs[0].Patch, "\r") {
		t.Fatalf("patch still contains CR; EOL not normalized: %q", diffs[0].Patch)
	}
}

func TestTrackedButIgnoredFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.json", "{}")
	writeFile(t, dir, ".gitignore", "/config.json\n")

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("config.json"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@local", When: time.Now()}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}

	m := New(dir)
	defer m.Cleanup()

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("tracked-but-ignored file produced phantom diffs: %+v", diffs)
	}
}

func TestGitignoreMidSessionChange(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "k")
	writeFile(t, dir, "build/x", "x")

	m := New(dir)
	defer m.Cleanup()
	if err := m.ready(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, ".gitignore", "build/\n")
	writeFile(t, dir, "build/y", "y")

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}

	if d, ok := byPath[".gitignore"]; !ok || d.Status != StatusAdded {
		t.Fatalf("missing .gitignore diff: %+v", diffs)
	}
	if _, ok := byPath["build/y"]; ok {
		t.Fatalf("ignored build/y appears in diff: %+v", diffs)
	}
	if _, ok := byPath["build/x"]; ok {
		t.Fatalf("newly ignored build/x reported as deleted: %+v", diffs)
	}
}

func TestSymlinkAndExecBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not supported")
	}

	dir := t.TempDir()
	writeFile(t, dir, "target.txt", "t")
	writeFile(t, dir, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	m := New(dir)
	defer m.Cleanup()

	cps, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	baseline := cps[0].Hash

	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}

	diffs, err := m.DiffFromBaseline()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if _, ok := byPath["link"]; !ok {
		t.Fatalf("symlink change not detected: %+v", diffs)
	}
	if _, ok := byPath["run.sh"]; !ok {
		t.Fatalf("mode change not detected: %+v", diffs)
	}

	if err := m.Restore(baseline); err != nil {
		t.Fatal(err)
	}

	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "target.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("exec bit not restored")
	}
}

func TestCleanupDoesNotBlockOnInFlightOp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")

	m := New(dir)
	if _, err := m.DiffFromBaseline(); err != nil {
		t.Fatal(err)
	}

	prev := cleanupTimeout
	cleanupTimeout = 100 * time.Millisecond
	defer func() { cleanupTimeout = prev }()

	m.mu.Lock()
	start := time.Now()
	m.Cleanup()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Cleanup blocked for %v", elapsed)
	}
	m.mu.Unlock()
}
