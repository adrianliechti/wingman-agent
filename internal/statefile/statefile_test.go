package statefile

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestWriteReplacesFileWithPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte("new"), nil} {
		if err := Write(path, data); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("contents = %q, %v; want %q", got, err, data)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions = %v, want 0600", info.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("directory after writing = %v, %v", entries, err)
	}
}

func TestWriteCleansUpAfterFailedRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("replacement")); err == nil {
		t.Fatal("replaced a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" || !entries[0].IsDir() {
		t.Fatalf("failed write changed destination or left temporary files: %v, %v", entries, err)
	}
}

func TestConcurrentWritesUseSeparateTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var writers sync.WaitGroup
	for i := range 16 {
		writers.Go(func() {
			data := bytes.Repeat([]byte{byte(i)}, 64<<10)
			if err := Write(path, data); err != nil {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 64<<10 || !bytes.Equal(data, bytes.Repeat(data[:1], len(data))) {
		t.Fatal("concurrent writers published incomplete or mixed contents")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("concurrent writes left temporary files: %v, %v", entries, err)
	}
}
