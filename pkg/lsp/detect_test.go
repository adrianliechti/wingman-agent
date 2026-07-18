package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFindCommandIn(t *testing.T) {
	dir := t.TempDir()
	name := "gopls"
	if runtime.GOOS == "windows" {
		name = "gopls.exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := findCommandIn([]string{t.TempDir(), dir}, "gopls"); got != path {
		t.Fatalf("findCommandIn = %q, want %q", got, path)
	}

	if got := findCommandIn([]string{dir}, "rust-analyzer"); got != "" {
		t.Fatalf("findCommandIn for missing command = %q, want empty", got)
	}
}

func TestResolveCommandFindsVenvServers(t *testing.T) {
	binSub := filepath.Join(".venv", "bin")
	fileName := "pylsp"
	if runtime.GOOS == "windows" {
		binSub = filepath.Join(".venv", "Scripts")
		fileName = "pylsp.exe"
	}

	root := t.TempDir()
	proj := filepath.Join(root, "services", "api")
	binDir := filepath.Join(proj, binSub)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, fileName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := resolveCommand(proj, root, "pylsp"); got != path {
		t.Fatalf("resolveCommand = %q, want %q", got, path)
	}

	// A venv at the workspace root serves nested project dirs via walk-up.
	rootBin := filepath.Join(root, binSub)
	if err := os.MkdirAll(rootBin, 0o755); err != nil {
		t.Fatal(err)
	}
	rootServer := filepath.Join(rootBin, "basedpyright-langserver")
	if runtime.GOOS == "windows" {
		rootServer += ".cmd"
	}
	if err := os.WriteFile(rootServer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := resolveCommand(proj, root, "basedpyright-langserver"); got != rootServer {
		t.Fatalf("walk-up resolveCommand = %q, want %q", got, rootServer)
	}
}

func TestCommandCandidates(t *testing.T) {
	got := commandCandidates("windows", "gopls")
	want := []string{"gopls.exe", "gopls.cmd", "gopls.bat", "gopls"}
	if len(got) != len(want) {
		t.Fatalf("windows candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("windows candidates = %v, want %v", got, want)
		}
	}

	if got := commandCandidates("darwin", "gopls"); len(got) != 1 || got[0] != "gopls" {
		t.Fatalf("darwin candidates = %v, want [gopls]", got)
	}
}

func TestFindCommandInIgnoresNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits are not checked on windows")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gopls"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "rust-analyzer"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := findCommandIn([]string{dir}, "gopls"); got != "" {
		t.Fatalf("non-executable file resolved: %q", got)
	}
	if got := findCommandIn([]string{dir}, "rust-analyzer"); got != "" {
		t.Fatalf("directory resolved: %q", got)
	}
}
