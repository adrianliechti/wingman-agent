package acptest

import (
	"os"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

// ServeTCK exposes a real bridge with its deterministic contract backend over
// stdio. The upstream TCK launches a fresh test executable for every scenario.
func ServeTCK(t *testing.T, factory Factory) {
	t.Helper()
	if os.Getenv("ACP_TCK_AGENT") != "1" {
		t.Skip("run scripts/test-acp-tck.sh to use the upstream ACP TCK")
	}
	// Keep Go's test summary off the protocol stream, including on failure.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	agent := factory(t)
	if c, ok := agent.(closer); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	conn := acp.NewAgentConnection(agent, stdout, os.Stdin)
	<-conn.Done()
}
