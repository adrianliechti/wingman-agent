package shell

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestEscalationVerdictsAreNeverReused(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "workspace")
	t.Setenv(workspaceSandboxActiveEnv, "")
	reviews := 0
	elicit := &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
		reviews++
		return tool.Verdict{Allowed: reviews == 1, Reason: "authorization revoked"}, nil
	}}
	m := NewExecManager(nil)
	defer m.Close()
	exec := ExecTools(m, t.TempDir(), elicit, nil)[0]
	args := map[string]any{"command": "echo harmless", "sandbox_permissions": "require_escalated", "justification": "fixture", "wait": 10}
	if _, err := exec.Execute(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(t.Context(), args); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("second execution = %v", err)
	}
	if reviews != 2 {
		t.Fatalf("reviews=%d", reviews)
	}
}

func TestPermissionRequestPreservesSandboxAndDoesNotMutateBase(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "workspace")
	t.Setenv(workspaceSandboxActiveEnv, "")
	t.Setenv(workspaceSandboxNetworkEnv, "off")
	workspace, extra, temp := t.TempDir(), t.TempDir(), t.TempDir()
	base := &Options{TempDir: temp}
	opts, mode, err := executionOptions(workspace, map[string]any{"sandbox_permissions": "require_permissions", "network": true, "writable_roots": []any{extra}, "justification": "download fixture"}, base)
	if err != nil {
		t.Fatal(err)
	}
	var action map[string]any
	elicit := &tool.Elicitation{Review: func(_ context.Context, req tool.Approval) (tool.Verdict, error) {
		action = req.Action
		return tool.Verdict{Allowed: true}, nil
	}}
	if err := confirmExecution(t.Context(), elicit, "echo fixture", workspace, workspace, mode, "download fixture", opts); err != nil {
		t.Fatal(err)
	}
	policy := action["execution_policy"].(map[string]any)
	if policy["sandboxed"] != true || policy["network"] != true || action["type"] != "sandbox_permissions" {
		t.Fatalf("wrong evidence: %#v", action)
	}
	if !slices.Contains(policy["writable_roots"].([]string), canonicalPath(extra)) {
		t.Fatal("missing requested root")
	}
	if base.Network || len(base.ExtraWritableRoots) != 0 {
		t.Fatal("permission grant leaked into later commands")
	}
	if mode == "require_escalated" {
		t.Fatal("network grant removed the sandbox")
	}
}

func TestMalformedPermissionRequestsFailBeforeApproval(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "workspace")
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace := t.TempDir()
	for _, args := range []map[string]any{
		{"sandbox_permissions": "typo"},
		{"network": true},
		{"writable_roots": []any{t.TempDir()}},
		{"sandbox_permissions": "require_permissions", "network": true},
		{"sandbox_permissions": "require_permissions", "justification": "none"},
		{"sandbox_permissions": "require_permissions", "justification": "broad", "writable_roots": []any{filepath.VolumeName(workspace) + string(filepath.Separator)}},
		{"sandbox_permissions": "require_permissions", "justification": "shared tmp", "writable_roots": []any{os.TempDir()}},
	} {
		if _, _, err := executionOptions(workspace, args, nil); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestSandboxNetworkAndTempAreRestrictedByDefault(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	if sandboxNetworkEnabled() {
		t.Fatal("network enabled by default")
	}
	roots := writableRootsFor(t.TempDir(), nil)
	for _, root := range roots {
		if root == canonicalPath(os.TempDir()) || root == canonicalPath("/tmp") {
			t.Fatalf("shared temp granted: %v", roots)
		}
	}
}

func TestPermissionExpansionsCannotMasqueradeAsReadOnly(t *testing.T) {
	for _, mode := range []string{"require_permissions", "require_escalated"} {
		if effect := ClassifyEffect(map[string]any{"command": "echo harmless", "sandbox_permissions": mode}); effect != tool.EffectDangerous {
			t.Fatalf("%s classified as %v", mode, effect)
		}
	}
}

func TestScopedRootCannotSelectProtectedConfiguration(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "workspace")
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace, outside := t.TempDir(), t.TempDir()
	for _, name := range []string{".git", ".agents", ".codex", ".vscode"} {
		root := filepath.Join(outside, name)
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := executionOptions(workspace, map[string]any{"sandbox_permissions": "require_permissions", "writable_roots": []any{root}, "justification": "fixture"}, nil); err == nil {
			t.Errorf("granted protected directory %s", root)
		}
	}
}

func TestPermissionReviewRejectsChangedRootAlias(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "workspace")
	t.Setenv(workspaceSandboxActiveEnv, "")
	first, second, parent := t.TempDir(), t.TempDir(), t.TempDir()
	alias := filepath.Join(parent, "workspace")
	if err := os.Symlink(first, alias); err != nil {
		t.Skip(err)
	}
	elicit := &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, alias); err != nil {
			t.Fatal(err)
		}
		return tool.Verdict{Allowed: true}, nil
	}}
	if err := confirmExecution(t.Context(), elicit, "echo fixture", alias, alias, "require_permissions", "fixture", &Options{Network: true}); err == nil {
		t.Fatal("approval followed a changed workspace alias")
	}
}

func TestReviewOnlyApproverIsUsedAndMissingApproverFailsClosed(t *testing.T) {
	calls := 0
	elicit := &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
		calls++
		return tool.Verdict{Allowed: true}, nil
	}}
	if err := confirmIfDangerous(t.Context(), elicit, "terminal_input", "rm -rf fixture", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("review-only approver bypassed")
	}
	if err := confirmIfDangerous(t.Context(), nil, "terminal_input", "rm -rf fixture", true); err == nil {
		t.Fatal("missing approver allowed execution")
	}
}
