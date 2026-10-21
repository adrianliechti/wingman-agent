package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
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

	escalated, err := exec.Execute(context.Background(), map[string]any{
		"command": touchCommand(outside), "wait": 10,
		"sandbox_permissions": "require_escalated", "justification": "write a marker in the home directory",
	})
	if err != nil || strings.Contains(escalated.Content, "sandbox_apply: Operation not permitted") {
		if runtime.GOOS == "windows" {
			t.Fatal(err)
		}
		t.Skipf("sandbox backend unavailable: %v %s", err, escalated.Content)
	}
	if escalated.IsError {
		t.Fatalf("escalated command failed: %s", escalated.Content)
	}
	if len(approvals) != 1 || approvals[0].Action["type"] != "sandbox_escalation" || !strings.Contains(approvals[0].Message, "write a marker") {
		t.Fatalf("approvals = %+v", approvals)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("escalated command did not run outside the sandbox: %v", err)
	}
}

// A command the sandbox blocks is re-run outside it once that is approved.
func TestExecRetriesSandboxDenialOutsideSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no sandbox backend on this platform")
	}
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	outside := filepath.Join(home, fmt.Sprintf(".wingman-retry-test-%d", os.Getpid()))
	defer os.Remove(outside)

	type reviewed struct {
		approval tool.Approval
		call     tool.ToolCall
	}
	review := func(verdict tool.Verdict) func(*[]reviewed) *tool.Elicitation {
		return func(reviews *[]reviewed) *tool.Elicitation {
			return &tool.Elicitation{Review: func(ctx context.Context, req tool.Approval) (tool.Verdict, error) {
				call, _ := hook.ToolCallFromContext(ctx)
				*reviews = append(*reviews, reviewed{req, call})
				return verdict, nil
			}}
		}
	}
	for _, tc := range []struct {
		name       string
		elicit     func(*[]reviewed) *tool.Elicitation
		validation bool
		allowed    bool
		reviews    int
		notice     string
	}{
		{"approved", review(tool.Verdict{Allowed: true}), false, true, 1, "re-running the command outside the sandbox"},
		{"declined", review(tool.Verdict{Reason: "fixture denial"}), false, false, 1, "not approved: fixture denial"},
		{"no approver", func(*[]reviewed) *tool.Elicitation { return nil }, false, false, 0, "not approved: command requires approval"},
		{"validation", review(tool.Verdict{Allowed: true}), true, false, 0, "Possible sandbox denial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer os.Remove(outside)
			var reviews []reviewed
			manager := NewExecManager(nil)
			defer manager.Close()
			args := map[string]any{"command": touchCommand(outside), "wait": 10, "validation": tc.validation}
			raw, _ := json.Marshal(args)
			ctx := hook.WithToolCall(context.Background(), tool.ToolCall{ID: "call-1", Name: "exec", Args: string(raw)})
			result, err := ExecTools(manager, t.TempDir(), tc.elicit(&reviews), nil)[0].Execute(ctx, args)
			if err != nil || strings.Contains(result.Content, "sandbox_apply: Operation not permitted") {
				if runtime.GOOS == "windows" {
					t.Fatal(err)
				}
				t.Skipf("sandbox backend unavailable: %v %s", err, result.Content)
			}
			if result.IsError == tc.allowed || !strings.Contains(result.Content, tc.notice) {
				t.Fatalf("result = %+v", result)
			}
			if _, err := os.Stat(outside); (err == nil) != tc.allowed {
				t.Fatalf("outside write after %s retry: %v", tc.name, err)
			}
			if len(reviews) != tc.reviews {
				t.Fatalf("reviews = %+v", reviews)
			}
			if tc.reviews == 0 {
				return
			}
			approval := reviews[0].approval
			if denial, _ := approval.Action["sandbox_denial"].(string); approval.Action["type"] != "sandbox_escalation" || denial == "" || !strings.Contains(approval.Message, "the sandbox blocked it: "+denial) {
				t.Fatalf("retry approval lacks the sandbox denial: %+v", approval)
			}
			// Permission hooks judge the escalated call, not the sandboxed one.
			if !strings.Contains(reviews[0].call.Args, `"sandbox_permissions":"require_escalated"`) {
				t.Fatalf("permission hooks see the sandboxed call: %s", reviews[0].call.Args)
			}
		})
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
