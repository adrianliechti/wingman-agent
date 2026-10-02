//go:build !windows

package shell

import (
	"strings"
	"testing"
)

// Ported from pi's bash tool tests: a command killed by a signal is an error
// result that keeps the output produced before the kill and reports the
// termination instead of inventing an exit code.
func TestExecCommandReportsSignalTerminationWithPartialOutput(t *testing.T) {
	result := runExec(t, `printf 'before-kill\n'; kill -KILL $$`)
	if !result.IsError || !strings.Contains(result.Content, "before-kill") || !strings.Contains(result.Content, "Command terminated (signal: killed)") {
		t.Fatalf("signal-killed result = %+v", result)
	}
	if code, ok := result.Metadata["exit_code"].(int); !ok || code >= 0 {
		t.Fatalf("exit_code = %v, want a negative signal placeholder", result.Metadata["exit_code"])
	}
}
