//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Verify real side effects in disposable directories, rather than relying
// only on the classifier's own understanding of command-line syntax.
func TestReadOnlyRulesMatchToolSideEffects(t *testing.T) {
	for _, name := range []string{"uniq", "xxd"} {
		t.Run(name, func(t *testing.T) {
			exe, err := exec.LookPath(name)
			if err != nil {
				t.Skip(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "input"), []byte("fixture\nfixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "input", "output")
			cmd.Dir = dir
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(dir, "output")); err != nil {
				t.Fatal("fixture did not demonstrate the output-file side effect")
			}
			if IsReadOnlyCommand(name + " input output") {
				t.Fatal("observed filesystem write was classified read-only")
			}
		})
	}
}

func TestNestedEnvironmentWrapperRunsStartupCode(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "startup"), []byte("printf fixture > marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := `command env BASH_ENV="$PWD/startup" bash -c ':'`
	cmd := exec.Command(bash, "-c", source)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Fatal("fixture did not execute the startup hook")
	}
	if !IsDangerousCommand(source) {
		t.Fatal("startup execution bypassed review")
	}
}
