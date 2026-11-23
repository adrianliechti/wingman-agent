package tooling

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnableRejectsMissingShebangInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows launchers do not use shebangs")
	}
	command := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(command, []byte("#!/missing/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Runnable(command) {
		t.Fatal("launcher with a missing interpreter is runnable")
	}
}

func TestEnvironmentPrependsAbsoluteCommandDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	command := filepath.Join(directory, "tool")
	environment := Environment(command, []string{"PATH=/usr/bin", "EXAMPLE=1"})
	want := "PATH=" + directory + string(os.PathListSeparator) + "/usr/bin"
	if environment[0] != want {
		t.Fatalf("environment PATH = %q, want %q", environment[0], want)
	}
}

func TestEnvironmentDoesNotAddCurrentDirectoryToEmptyPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	command := filepath.Join(directory, "tool")
	environment := Environment(command, []string{"PATH="})
	if want := "PATH=" + directory; environment[0] != want {
		t.Fatalf("environment PATH = %q, want %q", environment[0], want)
	}
}

func TestLookPathSearchesEveryGOPATHEntry(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	directory := filepath.Join(second, "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	name := Candidates(runtime.GOOS, "wingman-gopath-test")[0]
	command := filepath.Join(directory, name)
	if err := os.WriteFile(command, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", strings.Join([]string{first, second}, string(os.PathListSeparator)))
	t.Setenv("PATH", "")
	got, err := LookPath("wingman-gopath-test")
	if err != nil {
		t.Fatal(err)
	}
	if got != command {
		t.Fatalf("LookPath = %q, want %q", got, command)
	}
}

func TestLookPathRejectsMissingAbsoluteCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := LookPath(path); err != exec.ErrNotFound {
		t.Fatalf("LookPath error = %v, want exec.ErrNotFound", err)
	}
}

func TestEnvironmentAddsUserDirectoryShebangInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows launchers do not use shebangs")
	}
	interpreterDir := t.TempDir()
	interpreter := filepath.Join(interpreterDir, "wingman-test-interp")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOBIN", interpreterDir)

	command := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(command, []byte("#!/usr/bin/env wingman-test-interp\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !Runnable(command) {
		t.Fatal("launcher with a user-directory interpreter is not runnable")
	}
	environment := Environment(command, []string{"PATH=/usr/bin"})
	if !pathContains(strings.TrimPrefix(environment[0], "PATH="), interpreterDir) {
		t.Fatalf("environment PATH %q misses interpreter directory %q", environment[0], interpreterDir)
	}
}

func TestEnvironmentUsesCaseSensitivePathKeyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows environment keys are case insensitive")
	}
	directory := filepath.Join(t.TempDir(), "bin")
	command := filepath.Join(directory, "tool")
	environment := Environment(command, []string{"Path=/wrong", "PATH=/usr/bin"})
	if environment[0] != "Path=/wrong" {
		t.Fatalf("unrelated Path variable changed to %q", environment[0])
	}
	want := "PATH=" + directory + string(os.PathListSeparator) + "/usr/bin"
	if environment[1] != want {
		t.Fatalf("environment PATH = %q, want %q", environment[1], want)
	}
}
