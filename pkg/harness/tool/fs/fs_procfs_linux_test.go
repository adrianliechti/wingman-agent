package fs_test

import (
	"context"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

// Unconfined reads run in the agent's process, so /proc/self is the agent, not a sandboxed command; refuse its environment before reading any bytes.
func TestUnconfinedReadsDoNotDiscloseTheAgentEnvironment(t *testing.T) {
	root := openRoot(t, t.TempDir())
	if out, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": "/proc/self/environ"}); err == nil || strings.Contains(out.Content.Text(), "PATH=") {
		t.Errorf("read of the agent's environment = %q, %v", out.Content.Text(), err)
	}
	for _, path := range []string{"/proc/self/environ", "/proc/self"} {
		out, _ := GrepTool(root).Execute(context.Background(), map[string]any{"path": path, "glob": "environ", "pattern": "PATH=", "output_mode": "content"})
		if strings.Contains(out.Content.Text(), "PATH=") {
			t.Errorf("grep of %s disclosed the agent's environment: %q", path, out.Content.Text())
		}
	}
}
