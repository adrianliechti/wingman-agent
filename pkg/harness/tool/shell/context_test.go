package shell

import (
	"strings"
	"testing"
)

func TestExecutionContextPreservesInheritedSandbox(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "1")
	for _, opts := range []*Options{nil, {DisableSandbox: true}} {
		got := ExecutionContext(t.TempDir(), opts)
		if !strings.Contains(got, "inherited") || strings.Contains(got, "disabled") {
			t.Fatalf("context claims inherited sandbox can be disabled: %s", got)
		}
	}
}

func TestExecutionContextKeepsPrivateTempWithDisabledOrInheritedSandbox(t *testing.T) {
	for _, inherited := range []string{"", "1"} {
		t.Setenv(workspaceSandboxActiveEnv, inherited)
		opts := &Options{TempDir: t.TempDir(), DisableSandbox: true}
		got := ExecutionContext(t.TempDir(), opts)
		if !strings.Contains(got, "TMPDIR = TEMP = TMP = `"+opts.TempDir+"`") {
			t.Fatalf("context omits the command's private temp: %s", got)
		}
		if strings.Contains(got, "Shell writable roots:") || strings.Contains(got, "Shell IP network:") || strings.Contains(got, "Escalated") {
			t.Fatalf("context advertises an inactive local policy: %s", got)
		}
	}
}

func TestExecutionContextNotesHostTempForEscalation(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	got := ExecutionContext(t.TempDir(), &Options{TempDir: t.TempDir()})
	if !strings.Contains(got, "Escalated commands use the host temporary directory") {
		t.Fatalf("context implies escalated commands share private temp: %s", got)
	}
}
