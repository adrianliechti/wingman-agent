package rewind

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
