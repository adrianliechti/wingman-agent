package schedule

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

// Options applies the same capabilities and approvals as interactive execution.
type Options struct {
	WorkDir     string
	Disabled    bool
	Elicitation *tool.Elicitation
	Approvals   *shell.Approvals
	Shell       *shell.Options
}

func scriptApproval(dir, script string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(dir+"\x00"+script)))
}

func approveScript(ctx context.Context, dir, script string, opts *Options) error {
	if script == "" {
		return nil
	}
	if opts != nil && opts.Disabled {
		return fmt.Errorf("scheduled scripts are disabled because shell access is disabled")
	}
	if !shell.IsDangerousCommand(script) {
		return nil
	}
	if opts == nil || opts.Elicitation == nil || opts.Elicitation.Confirm == nil {
		return fmt.Errorf("scheduled script requires user approval before execution")
	}
	approvals := opts.Approvals
	if approvals == nil {
		approvals = shell.NewApprovals()
	}
	return shell.ConfirmCommand(ctx, script, dir, opts.Elicitation, approvals)
}

// RunTaskGate accepts only the exact script approved when the task was saved.
// Legacy or edited scripts must pass the current policy before they can run.
func RunTaskGate(ctx context.Context, dir string, task Task, opts *Options) (bool, string, error) {
	if opts != nil && opts.Disabled {
		return gateFailure(fmt.Errorf("scheduled scripts are disabled because shell access is disabled"))
	}
	if task.ScriptApproval != scriptApproval(dir, task.Script) {
		if err := approveScript(ctx, dir, task.Script, opts); err != nil {
			return gateFailure(err)
		}
	}
	return runGate(ctx, dir, task.Script, opts)
}

func gateFailure(err error) (bool, string, error) {
	return true, fmt.Sprintf("pre-check script did not run: %v", err), err
}
