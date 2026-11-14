package shell

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFailedCommandIsNotADenialBecauseOfGoCacheWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	failed := exec.Command("sh", "-c", "exit 1").Run()
	const (
		warning      = "go: writing stat cache: open /home/u/go/pkg/mod/cache/download/example.com/shop/@v/v0.0.0-1.info1.tmp: operation not permitted\n"
		linuxWarning = "go: writing stat cache: open /home/u/go/pkg/mod/cache/download/example.com/shop/@v/v0.0.0-1.info1.tmp: read-only file system\n"
		compileError = "catalog/product.go:23:9: undefined: util\n"
		denial       = "touch: /etc/hosts: Operation not permitted"
	)
	for _, tc := range []struct{ name, output, want string }{
		{"warning before a compile error", warning + compileError, ""},
		{"linux spelling", linuxWarning + "FAIL\texample.com/shop/catalog [build failed]\n", ""},
		{"indented warning", "  " + warning + compileError, ""},
		{"warning and a real denial", warning + denial + "\n", denial},
		{"real denial", denial + "\n", denial},
	} {
		if got := sandboxDenial(tc.output, failed, true); got != tc.want {
			t.Errorf("%s: sandboxDenial = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestZshPassesUnmatchedGlobsThroughLikeBash(t *testing.T) {
	if got := shellArguments("/bin/bash", "true"); !slices.Equal(got, []string{"-c", "true"}) {
		t.Fatalf("bash arguments = %v", got)
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	t.Setenv("SHELL", zsh)
	t.Setenv(workspaceSandboxActiveEnv, "")
	workDir := t.TempDir()
	// Models write this for bash; zsh would abort the whole command with "no matches found".
	const command = "echo --include=*.nomatch-probe && echo reached"
	for name, opts := range map[string]*Options{"unsandboxed": {DisableSandbox: true}, "sandboxed": {}} {
		cmd, err := buildToolCommand(t.Context(), command, workDir, workDir, opts, false)
		if err != nil {
			t.Logf("%s: skipped: %v", name, err)
			continue
		}
		out, err := cmd.CombinedOutput()
		launcherFailed := slices.ContainsFunc(sandboxLauncherPrefixes, func(prefix string) bool { return strings.HasPrefix(string(out), prefix) })
		if err != nil && name == "sandboxed" && launcherFailed {
			t.Logf("sandbox unavailable: %s", out)
			continue
		}
		if err != nil || strings.TrimSpace(string(out)) != "--include=*.nomatch-probe\nreached" {
			t.Errorf("%s: output=%q err=%v", name, out, err)
		}
	}
}

func TestExecutionContextNamesBSDUtilitiesOnlyOnMacOS(t *testing.T) {
	got := ExecutionContext(t.TempDir(), &Options{})
	if named := strings.Contains(got, "BSD versions"); named != (runtime.GOOS == "darwin") {
		t.Fatalf("BSD note present=%t on %s:\n%s", named, runtime.GOOS, got)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(got, `no `+"`\\b`") {
		t.Fatalf("note does not name the unsupported word boundary:\n%s", got)
	}
}
