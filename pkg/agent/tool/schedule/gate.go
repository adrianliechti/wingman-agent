package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

const (
	gateTimeout   = 2 * time.Minute
	gateMaxOutput = 16 * 1024
)

// RunGate executes a task's pre-check script. It reports whether the agent
// should be woken; on script failure it fails open so the agent can fix it,
// and returns the error so the scheduler can back off repeat failures.
func RunGate(ctx context.Context, dir, script string, options ...*Options) (bool, string, error) {
	var opts *Options
	if len(options) > 0 {
		opts = options[0]
	}
	if err := approveScript(ctx, dir, script, opts); err != nil {
		return gateFailure(err)
	}
	return runGate(ctx, dir, script, opts)
}

func runGate(ctx context.Context, dir, script string, opts *Options) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()

	var shellOpts *shell.Options
	if opts != nil {
		shellOpts = opts.Shell
	}
	var out gateOutput
	cmd := shell.ToolCommand(ctx, script, dir, shellOpts)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	output := out.String()

	if err != nil {
		return true, fmt.Sprintf("pre-check script failed (%v); fix the script or remove it from the task.\n%s", err, output), err
	}

	if wake, ok := parseGateOutput(output); ok {
		return wake, output, nil
	}

	lines := strings.Split(output, "\n")
	if wake, ok := parseGateOutput(lines[len(lines)-1]); ok {
		return wake, output, nil
	}

	return true, output, nil
}

// Retain the tail so the final JSON wake decision survives verbose scripts.
type gateOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *gateOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= gateMaxOutput {
		b.data = append(b.data[:0], p[n-gateMaxOutput:]...)
		b.truncated = true
	} else {
		if excess := len(b.data) + n - gateMaxOutput; excess > 0 {
			b.data = b.data[excess:]
			b.truncated = true
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *gateOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if b.truncated {
		return "[earlier output truncated]\n" + text
	}
	return text
}

func parseGateOutput(s string) (bool, bool) {
	var result struct {
		Wake *bool `json:"wake"`
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &result); err != nil || result.Wake == nil {
		return false, false
	}

	return *result.Wake, true
}
