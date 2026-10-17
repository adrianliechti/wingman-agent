package schedule

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

// Options applies the same capabilities and approvals as interactive execution.
type Options struct {
	WorkDir     string
	Disabled    bool
	Elicitation *tool.Elicitation
	Shell       *shell.Options
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
	if opts == nil || opts.Elicitation == nil || (opts.Elicitation.Confirm == nil && opts.Elicitation.Review == nil) {
		return fmt.Errorf("scheduled script requires user approval before execution")
	}
	return shell.ConfirmCommand(ctx, script, dir, opts.Elicitation, opts.Shell)
}

// RunTaskGate re-evaluates each execution. A persisted hash proves no current
// authorization: scripts, user instructions and session modes can change.
func RunTaskGate(ctx context.Context, dir string, task Task, opts *Options) (bool, string, error) {
	if strings.TrimSpace(task.Script) == "" {
		return true, "", nil
	}
	if dir == "" && opts != nil {
		dir = opts.WorkDir
	}
	if opts != nil && opts.Disabled {
		return gateFailure(fmt.Errorf("scheduled scripts are disabled because shell access is disabled"))
	}
	if err := approveScript(ctx, dir, task.Script, opts); err != nil {
		return gateFailure(err)
	}
	return runGate(ctx, dir, task.Script, opts)
}

func gateFailure(err error) (bool, string, error) {
	return true, fmt.Sprintf("pre-check script did not run: %v", err), err
}
