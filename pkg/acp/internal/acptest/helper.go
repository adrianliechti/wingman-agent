package acptest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helperExeName    = "acp-contract-helper.exe"
	helperTestSuffix = ".test-run"
	warmUpEnv        = "ACPTEST_WARM_UP"
)

// Windows cannot execute the #!/bin/sh wrapper CommandHelper writes elsewhere,
// so the test binary itself is installed as acp-contract-helper.exe next to a
// sidecar naming the helper test. Main rewrites os.Args when it starts under
// that name, mirroring the wrapper's `-test.run '^name$' -- "$@"`.
//
// Helpers are hard links beside the existing test executable, so they keep
// its executable permissions and do not need another first-launch scan.
// The links outlive individual tests because
// Windows refuses to unlink any name of a running image; Main removes them
// after all tests have finished.
var helpers struct {
	sync.Mutex
	main   bool
	dir    string
	master string
	paths  map[string]string
}

// Main runs the tests of a package that uses CommandHelper. Call it from
// TestMain: os.Exit(acptest.Main(m)).
func Main(m *testing.M) int {
	exe, err := os.Executable()
	if err == nil && strings.EqualFold(filepath.Base(exe), helperExeName) {
		if name, err := os.ReadFile(exe + helperTestSuffix); err == nil {
			args := []string{os.Args[0], "-test.run=^" + strings.TrimSpace(string(name)) + "$", "--"}
			os.Args = append(args, os.Args[1:]...)
		}
	} else if err == nil && runtime.GOOS == "windows" && os.Getenv(warmUpEnv) == "" {
		// The first child launch of a fresh test binary can stall for many
		// seconds while it is scanned; absorb that before tests with
		// startup deadlines re-execute os.Args[0].
		warm := exec.Command(exe, "-test.run=^$")
		warm.Env = append(os.Environ(), warmUpEnv+"=1")
		_ = warm.Run()
	}
	helpers.Lock()
	helpers.main = true
	helpers.Unlock()
	code := m.Run()
	helpers.Lock()
	if helpers.dir != "" {
		// Exited helpers can keep their image locked briefly, so retry the
		// way testing.TempDir does on Windows.
		for deadline := time.Now().Add(5 * time.Second); os.RemoveAll(helpers.dir) != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
		}
	}
	helpers.Unlock()
	return code
}

func windowsCommandHelper(t *testing.T, testName string) string {
	t.Helper()
	helpers.Lock()
	defer helpers.Unlock()
	if !helpers.main {
		t.Fatal("acptest: CommandHelper on Windows requires TestMain to call acptest.Main")
	}
	if path, ok := helpers.paths[testName]; ok {
		return path
	}
	if helpers.dir == "" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		dir, err := os.MkdirTemp(filepath.Dir(self), "acptest-helpers-")
		if err != nil {
			t.Fatal(err)
		}
		helpers.dir, helpers.master, helpers.paths = dir, self, map[string]string{}
	}
	dir := filepath.Join(helpers.dir, testName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, helperExeName)
	if err := os.Link(helpers.master, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+helperTestSuffix, []byte(testName), 0o600); err != nil {
		t.Fatal(err)
	}
	helpers.paths[testName] = path
	return path
}
