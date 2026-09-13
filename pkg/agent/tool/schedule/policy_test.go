package schedule

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

func TestScheduleScriptsRequireTheExecutionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		disabled, approve bool
	}{{"denied", false, false}, {"approved", false, true}, {"disabled", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			asked := 0
			opts := &Options{WorkDir: t.TempDir(), Disabled: tc.disabled, Approvals: shell.NewApprovals(), Elicitation: &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { asked++; return tc.approve, nil }}}
			tools := Tools(store, opts)
			_, err := tools[0].Execute(t.Context(), map[string]any{"prompt": "check", "schedule": "every 1h", "script": "git clean -fdx"})
			if tc.disabled || !tc.approve {
				if err == nil {
					t.Fatal("unapproved script was scheduled")
				}
				tasks, _ := store.List()
				if len(tasks) != 0 {
					t.Fatal("denied task persisted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				tasks, _ := store.List()
				if len(tasks) != 1 || tasks[0].ScriptApproval != scriptApproval(opts.WorkDir, tasks[0].Script) {
					t.Fatal("approval not tied to saved script")
				}
			}
			if tc.disabled && asked != 0 {
				t.Fatal("asked about a disabled capability")
			}
			if !tc.disabled && asked != 1 {
				t.Fatalf("approval calls = %d", asked)
			}
		})
	}
}

func TestGateFiltersSecretsAndHonorsEnvironmentPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	t.Setenv("OPENAI_API_KEY", "regression-test-value")
	_, out, err := RunGate(t.Context(), t.TempDir(), `printf '%s' "${OPENAI_API_KEY-unset}"`)
	if err != nil || out != "unset" {
		t.Fatalf("default environment: %q, %v", out, err)
	}
	opts := &Options{Shell: &shell.Options{Environment: &shell.EnvironmentPolicy{Replace: map[string]string{"WINGMAN_GATE_TEST": "replacement"}}}}
	_, out, err = RunGate(t.Context(), t.TempDir(), `printf '%s' "$WINGMAN_GATE_TEST"`, opts)
	if err != nil || out != "replacement" {
		t.Fatalf("configured environment: %q, %v", out, err)
	}
}

func TestChangedOrLegacyDangerousGateCannotExecuteWithoutApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "preserve-me")
	if err := os.WriteFile(marker, []byte("user work"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"", scriptApproval(dir, "echo harmless")} {
		task := Task{Script: "rm -rf preserve-me", ScriptApproval: hash}
		_, out, err := RunTaskGate(t.Context(), dir, task, nil)
		if err == nil || !strings.Contains(out, "approval") {
			t.Fatalf("unapproved gate: %q, %v", out, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("script ran without approval")
		}
	}
	task := Task{Script: "rm -rf preserve-me", ScriptApproval: scriptApproval(dir, "rm -rf preserve-me")}
	if _, _, err := RunTaskGate(t.Context(), dir, task, &Options{Disabled: true}); err == nil {
		t.Fatal("saved approval bypassed disabled shell")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("disabled script ran")
	}
}

func TestVerboseGatePreservesDecisionAndBoundsBufferedOutput(t *testing.T) {
	var output gateOutput
	for range 1000 {
		if _, err := output.Write([]byte(strings.Repeat("verbose log ", 100))); err != nil {
			t.Fatal(err)
		}
	}
	output.Write([]byte("\n{\"wake\":false}\n"))
	text := output.String()
	if len(text) > gateMaxOutput+100 || !strings.Contains(text, "earlier output truncated") {
		t.Fatalf("unbounded output: %d", len(text))
	}
	lines := strings.Split(text, "\n")
	wake, ok := parseGateOutput(lines[len(lines)-1])
	if !ok || wake {
		t.Fatal("lost final wake decision")
	}
}
