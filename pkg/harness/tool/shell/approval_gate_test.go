package shell

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func countingReviewer(reviews *int) *tool.Elicitation {
	return &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
		*reviews++
		return tool.Verdict{Allowed: true}, nil
	}}
}

func TestSandboxDecidesWhichCommandsAreReviewed(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace := t.TempDir()
	for _, tc := range []struct {
		command                string
		sandboxed, unsandboxed bool
	}{
		{"git status", false, false},
		{"rm -rf node_modules", false, true},
		{"git branch -D feature", false, true},
		{"git checkout -- file.go", false, true},
		{"git reset --hard", true, true},
		{"rm -rf .", true, true},
		{"sudo make install", true, true},
		{"rm -rf " + filepath.ToSlash(workspace), true, true},
	} {
		for _, disabled := range []bool{false, true} {
			want := tc.sandboxed
			if disabled {
				want = tc.unsandboxed
			}
			opts := &Options{DisableSandbox: disabled}
			reviews := 0
			if _, err := confirmExecution(t.Context(), countingReviewer(&reviews), tc.command, workspace, workspace, "use_default", "", "", opts, approveThisRun); err != nil {
				t.Fatal(err)
			}
			if (reviews == 1) != want {
				t.Errorf("%q with the sandbox disabled=%t: %d reviews, want reviewed=%t", tc.command, disabled, reviews, want)
			}
			// Without an approver the reviewed commands are refused and the rest run.
			if _, err := confirmExecution(t.Context(), nil, tc.command, workspace, workspace, "use_default", "", "", opts, approveThisRun); (err != nil) != want {
				t.Errorf("%q with the sandbox disabled=%t and no approver: %v", tc.command, disabled, err)
			}
			if got := RequiresApproval(tc.command, opts, workspace); got != want {
				t.Errorf("RequiresApproval(%q) with the sandbox disabled=%t = %t", tc.command, disabled, got)
			}
		}
	}
}

func TestPermissionRequestsAreReviewedWhateverTheCommand(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace := t.TempDir()
	for mode, opts := range map[string]*Options{"require_permissions": {Network: true}, "require_escalated": {}} {
		reviews := 0
		if _, err := confirmExecution(t.Context(), countingReviewer(&reviews), "git status", workspace, workspace, mode, "fixture", "", opts, approveThisRun); err != nil {
			t.Fatal(err)
		}
		if reviews != 1 {
			t.Errorf("%s ran a harmless command after %d reviews", mode, reviews)
		}
	}
}

// A process started inside another Wingman sandbox applies none of its own, so
// it keeps the full classifier.
func TestInheritedSandboxKeepsTheFullClassifier(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "1")
	reviews := 0
	if err := ConfirmCommand(t.Context(), "rm -rf node_modules", t.TempDir(), countingReviewer(&reviews)); err != nil {
		t.Fatal(err)
	}
	if reviews != 1 {
		t.Fatalf("%d reviews", reviews)
	}
}

func TestSandboxDecidesWhichSessionInputIsReviewed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// The last write, or closing stdin after them, is the one judged.
		inputs                 []string
		eof                    bool
		sandboxed, unsandboxed bool
	}{
		{name: "plain line", inputs: []string{"ls\n"}},
		{name: "named removal", inputs: []string{"rm -rf node_modules\n"}, unsandboxed: true},
		{name: "tab completion", inputs: []string{"ls sr\t"}, unsandboxed: true},
		{name: "arrow key recall", inputs: []string{"\x1b[A\n"}, unsandboxed: true},
		{name: "line after a control key", inputs: []string{"ls sr\t", "\n"}, unsandboxed: true},
		{name: "routine line pending at eof", inputs: []string{"rm -rf build"}, eof: true, unsandboxed: true},
		{name: "hard reset", inputs: []string{"git reset --hard\n"}, sandboxed: true, unsandboxed: true},
		{name: "workspace removal", inputs: []string{"rm -rf .\n"}, sandboxed: true, unsandboxed: true},
		{name: "removal split across writes", inputs: []string{"rm -r", "f .\n"}, sandboxed: true, unsandboxed: true},
		{name: "removal after a correction", inputs: []string{"rm -rf ./build\b\b\b\b\b\b\n"}, sandboxed: true, unsandboxed: true},
		{name: "removal submitted by a control key", inputs: []string{"rm -rf .", "\x04"}, sandboxed: true, unsandboxed: true},
		{name: "elevation pending at eof", inputs: []string{"sudo make install"}, eof: true, sandboxed: true, unsandboxed: true},
		{name: "long line", inputs: []string{strings.Repeat("a", maxClassifiableBytes+1) + "\n"}, unsandboxed: true},
		{name: "long destructive line", inputs: []string{strings.Repeat("a", maxClassifiableBytes+1) + "; rm -rf .\n"}, sandboxed: true, unsandboxed: true},
		{name: "pending line cut at the limit", inputs: []string{strings.Repeat("a", maxClassifiableBytes+1), "\n"}, sandboxed: true, unsandboxed: true},
	} {
		for _, sandboxed := range []bool{true, false} {
			want := tc.unsandboxed
			if sandboxed {
				want = tc.sandboxed
			}
			m := NewExecManager(nil)
			s := &execSession{sandboxed: sandboxed, tty: true, stdin: &reviewWriteCloser{}, done: make(chan struct{}), cancel: func() {}}
			reviews, before := 0, 0
			elicit := countingReviewer(&reviews)
			for _, input := range tc.inputs {
				before = reviews
				if _, err := applySessionInput(t.Context(), m, s, elicit, input, false); err != nil {
					t.Fatal(err)
				}
			}
			if tc.eof {
				before = reviews
				if _, err := applySessionInput(t.Context(), m, s, elicit, "", true); err != nil {
					t.Fatal(err)
				}
			}
			if got := reviews > before; got != want {
				t.Errorf("%s, sandboxed=%t: reviewed=%t, want %t", tc.name, sandboxed, got, want)
			}
			m.Close()
		}
	}
}

// Seatbelt refuses protected names by itself. Where the sandbox cannot,
// commands that would create protected configuration are still reviewed.
func TestSandboxedGateReviewsProtectedConfigurationWhereTheSandboxCannotBlockIt(t *testing.T) {
	enforced := protectedNamesEnforced
	t.Cleanup(func() { protectedNamesEnforced = enforced })
	for _, tc := range []struct {
		command  string
		reviewed bool // when the sandbox does not enforce protected names
	}{
		{"mkdir -p .vscode && echo '{}' > .vscode/tasks.json", true},
		{"echo 'export X=1' >> sub/.envrc", true},
		{"cp hooks.json .claude/settings.json", true},
		{"cat .vscode/settings.json", false},
		{"rm -rf node_modules", false},
		{"PYTHONPATH=src pytest", false},
	} {
		protectedNamesEnforced = false
		if got := requiresApproval(tc.command, true); got != tc.reviewed {
			t.Errorf("without name enforcement, %q reviewed = %v, want %v", tc.command, got, tc.reviewed)
		}
		protectedNamesEnforced = true
		if requiresApproval(tc.command, true) {
			t.Errorf("with name enforcement, %q is reviewed", tc.command)
		}
	}
}
