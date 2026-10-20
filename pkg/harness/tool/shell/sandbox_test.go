package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestResolveWorkdirWorkspaceSandbox(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")

	workspace := t.TempDir()
	inside := filepath.Join(workspace, "inside")
	if err := os.Mkdir(inside, 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	if got, err := resolveWorkdir(workspace, map[string]any{"workdir": inside}, nil); err != nil || got != inside {
		t.Fatalf("inside workdir = %q, %v", got, err)
	}
	if _, err := resolveWorkdir(workspace, map[string]any{"workdir": outside}, nil); err == nil || !strings.Contains(err.Error(), "outside sandbox workspace") {
		t.Fatalf("outside workdir error = %v", err)
	}
}

func TestResolveWorkdirWorkspaceSandboxRejectsSymlinkEscape(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")

	workspace := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := resolveWorkdir(workspace, map[string]any{"workdir": link}, nil); err == nil || !strings.Contains(err.Error(), "outside sandbox workspace") {
		t.Fatalf("symlink workdir error = %v", err)
	}
}

func TestResolveWorkdirAllowsConfiguredRootsWithoutNewPermission(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace, memory, temp, outside := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	opts := &Options{TempDir: temp, ExtraWritableRoots: []string{memory}}
	for _, root := range []string{memory, temp} {
		if got, err := resolveWorkdir(workspace, map[string]any{"workdir": root}, opts); err != nil || got != root {
			t.Errorf("configured workdir %q = %q, %v", root, got, err)
		}
	}
	link := filepath.Join(temp, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveWorkdir(workspace, map[string]any{"workdir": link}, opts); err == nil {
		t.Fatal("configured temp root allowed a symlink escape")
	}
}

func TestResolveWorkdirUsesOnlyValidatedPerCommandRoots(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace, outside := t.TempDir(), t.TempDir()
	args := map[string]any{"workdir": outside, "sandbox_permissions": "require_permissions", "writable_roots": []any{outside}, "justification": "test external project"}
	opts, _, err := executionOptions(workspace, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorkdir(workspace, args, opts); err != nil || got != outside {
		t.Fatalf("validated workdir = %q, %v", got, err)
	}
	if _, err := resolveWorkdir(workspace, args, nil); err == nil {
		t.Fatal("unvalidated root accepted directly from tool arguments")
	}
	if _, err := resolveWorkdir(workspace, map[string]any{"workdir": outside}, nil); err == nil {
		t.Fatal("per-command root granted to a later command")
	}
}

func TestWorkspaceSandboxIsDefaultAndRequiresExplicitOptOut(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	if !workspaceSandboxEnabled(nil) || !workspaceSandboxEnabled(&Options{}) {
		t.Fatal("sandbox should be enabled by default")
	}
	if workspaceSandboxEnabled(&Options{DisableSandbox: true}) {
		t.Fatal("sandbox enabled when DisableSandbox is true")
	}
}

func TestWorkspaceSandboxDoesNotNest(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "1")
	if workspaceSandboxEnabled(nil) {
		t.Fatal("sandbox should be inherited instead of nested")
	}
}

func TestWritableRootsForIncludesValidExtraRoots(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	roots := writableRootsFor(workspace, []string{extra, missing, "", extra}, extra)

	if !slices.Contains(roots, canonicalPath(workspace)) {
		t.Fatalf("roots missing workspace: %q", roots)
	}
	if !slices.Contains(roots, canonicalPath(extra)) {
		t.Fatalf("roots missing valid extra root: %q", roots)
	}
	if slices.Contains(roots, canonicalPath(missing)) {
		t.Fatalf("roots should drop a nonexistent extra root: %q", roots)
	}
	count := 0
	for _, r := range roots {
		if r == canonicalPath(extra) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("extra root should be de-duplicated, got %d entries: %q", count, roots)
	}
}

func TestBuildSandboxedCommandRejectsMissingWorkspace(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := buildSandboxedCommand(t.Context(), "/bin/sh", "true", missing, sandboxOptions{WorkspaceDir: missing}); err == nil || !strings.Contains(err.Error(), "not an accessible directory") {
		t.Fatalf("missing workspace error = %v", err)
	}
}

func TestExecSandboxConfinesWritesToWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no sandbox backend on this platform")
	}
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	outside := filepath.Join(home, fmt.Sprintf(".wingman-sandbox-test-%d", os.Getpid()))
	defer os.Remove(outside)

	manager := NewExecManager(nil)
	defer manager.Close()
	exec := ExecTools(manager, t.TempDir(), nil, nil)[0]
	run := func(command string) tool.Result {
		result, err := exec.Execute(context.Background(), map[string]any{"command": command, "wait": 60})
		if err != nil || strings.Contains(result.Content, "sandbox_apply: Operation not permitted") {
			if runtime.GOOS == "windows" {
				t.Fatal(err)
			}
			t.Skipf("sandbox backend unavailable: %v %s", err, result.Content)
		}
		return result
	}
	if inside := run(touchCommand("inside.txt")); inside.IsError {
		t.Fatalf("write inside the workspace failed: %s", inside.Content)
	}
	if escaped := run(touchCommand(outside)); !escaped.IsError {
		t.Fatalf("write outside the workspace was allowed: %s", escaped.Content)
	}
}

func TestExecEscalationRunsOutsideSandboxAfterApproval(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no sandbox backend on this platform")
	}
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	outside := filepath.Join(home, fmt.Sprintf(".wingman-escalation-test-%d", os.Getpid()))
	defer os.Remove(outside)

	var approvals []tool.Approval
	elicit := &tool.Elicitation{
		Confirm: func(context.Context, string) (bool, error) { return false, nil },
		Review: func(_ context.Context, req tool.Approval) (tool.Verdict, error) {
			approvals = append(approvals, req)
			return tool.Verdict{Allowed: true}, nil
		},
	}
	manager := NewExecManager(nil)
	defer manager.Close()
	exec := ExecTools(manager, t.TempDir(), elicit, nil)[0]
	if _, ok := exec.Parameters["properties"].(map[string]any)["sandbox_permissions"]; !ok {
		t.Fatal("sandboxed exec does not offer sandbox_permissions")
	}

	denied, err := exec.Execute(context.Background(), map[string]any{"command": touchCommand(outside), "wait": 10})
	if err != nil || strings.Contains(denied.Content, "sandbox_apply: Operation not permitted") {
		if runtime.GOOS == "windows" {
			t.Fatal(err)
		}
		t.Skipf("sandbox backend unavailable: %v %s", err, denied.Content)
	}
	if !denied.IsError || !strings.Contains(denied.Content, "require_escalated") {
		t.Fatalf("sandbox denial carries no escalation hint: %s", denied.Content)
	}
	escalated, err := exec.Execute(context.Background(), map[string]any{
		"command": touchCommand(outside), "wait": 10,
		"sandbox_permissions": "require_escalated", "justification": "write a marker in the home directory",
	})
	if err != nil || escalated.IsError {
		t.Fatalf("escalated command failed: %v %s", err, escalated.Content)
	}
	if len(approvals) != 1 || approvals[0].Action["type"] != "sandbox_escalation" || !strings.Contains(approvals[0].Message, "write a marker") {
		t.Fatalf("approvals = %+v", approvals)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("escalated command did not run outside the sandbox: %v", err)
	}
}

func TestExecEscalationFailsClosedWithoutApprover(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	manager := NewExecManager(nil)
	defer manager.Close()
	_, err := ExecTools(manager, t.TempDir(), nil, nil)[0].Execute(context.Background(), map[string]any{
		"command": "true", "sandbox_permissions": "require_escalated", "justification": "test approval unavailable",
	})
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("escalation without an approver = %v", err)
	}
}

func TestExecOffersEscalationOnlyWhenSandboxed(t *testing.T) {
	manager := NewExecManager(nil)
	defer manager.Close()
	if _, ok := ExecTools(manager, t.TempDir(), nil, &Options{DisableSandbox: true})[0].Parameters["properties"].(map[string]any)["sandbox_permissions"]; ok {
		t.Fatal("unsandboxed exec offers sandbox_permissions")
	}
}

// touchCommand creates path in the platform's tool shell.
func touchCommand(path string) string {
	if runtime.GOOS == "windows" {
		return "New-Item -ItemType File -Force -Path '" + path + "' | Out-Null"
	}
	return "touch '" + path + "'"
}

// sandboxTestHome returns the home directory for writes outside the
// workspace. Windows uses a temporary home so credential tests and approved
// outside writes operate on fixtures.
func sandboxTestHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		home := t.TempDir()
		t.Setenv("USERPROFILE", home)
		t.Setenv("WINGMAN_HOME", filepath.Join(home, ".wingman"))
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}
