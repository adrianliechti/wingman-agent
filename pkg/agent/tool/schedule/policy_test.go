package schedule

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

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
			opts := &Options{WorkDir: t.TempDir(), Disabled: tc.disabled, Elicitation: &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { asked++; return tc.approve, nil }}}
			tools := Tools(store, opts)
			_, err := tools[0].Execute(t.Context(), map[string]any{"action": "create", "prompt": "check", "schedule": "every 1h", "script": "git clean -fdx"})
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
				if len(tasks) != 1 || tasks[0].Script != "git clean -fdx" {
					t.Fatal("approved script was not saved")
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
	store := NewFileStore(t.TempDir())
	// Old approval hashes remain readable but confer no authorization.
	legacyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(dir)+"\x00rm -rf preserve-me")))
	if err := os.WriteFile(store.Path(), []byte("tasks:\n  - script: rm -rf preserve-me\n    script_approval: "+legacyHash+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.List()
	if err != nil || len(legacy) != 1 {
		t.Fatalf("load legacy schedule: %v, %v", legacy, err)
	}
	for _, task := range []Task{{Script: "rm -rf preserve-me"}, legacy[0]} {
		_, out, err := RunTaskGate(t.Context(), dir, task, nil)
		if err == nil || !strings.Contains(out, "approval") {
			t.Fatalf("unapproved gate: %q, %v", out, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("script ran without approval")
		}
	}
	task := legacy[0]
	if _, _, err := RunTaskGate(t.Context(), dir, task, &Options{Disabled: true}); err == nil {
		t.Fatal("saved approval bypassed disabled shell")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("disabled script ran")
	}
}

func TestScheduledScriptRechecksReviewAfterCreationAndExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "fixture")
	store := NewMemoryStore()
	calls := 0
	opts := &Options{WorkDir: dir, Elicitation: &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
		calls++
		return tool.Verdict{Allowed: calls <= 2, Reason: "new user work"}, nil
	}}}
	_, err := Tools(store, opts)[0].Execute(t.Context(), map[string]any{
		"action": "create", "prompt": "check", "schedule": "every 1h", "script": "rm -rf fixture",
	})
	if err != nil || calls != 1 {
		t.Fatalf("review-only schedule creation: calls=%d, err=%v", calls, err)
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 1 {
		t.Fatalf("saved tasks: %v, %v", tasks, err)
	}
	if err := os.WriteFile(marker, []byte("disposable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunTaskGate(t.Context(), dir, tasks[0], opts); err != nil || calls != 2 {
		t.Fatalf("first execution: calls=%d, err=%v", calls, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("approved script did not run: %v", err)
	}
	if err := os.WriteFile(marker, []byte("new user work"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunTaskGate(t.Context(), dir, tasks[0], opts); err == nil || !strings.Contains(err.Error(), "new user work") || calls != 3 {
		t.Fatalf("repeated execution: calls=%d, err=%v", calls, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "new user work" {
		t.Fatalf("denied script changed user work: %q, %v", data, err)
	}
}

func TestVerboseGatePreservesDecisionAndBoundsBufferedOutput(t *testing.T) {
	var output gateOutput
	for range 1000 {
		if _, err := output.Write([]byte(strings.Repeat("verbose € log ", 100))); err != nil {
			t.Fatal(err)
		}
	}
	output.Write([]byte("\n{\"wake\":false}\n"))
	text := output.String()
	if len(text) > gateMaxOutput+100 || !strings.Contains(text, "earlier output truncated") || !utf8.ValidString(text) {
		t.Fatalf("unbounded or invalid UTF-8 output: %d", len(text))
	}
	lines := strings.Split(text, "\n")
	wake, ok := parseGateOutput(lines[len(lines)-1])
	if !ok || wake {
		t.Fatal("lost final wake decision")
	}
}
