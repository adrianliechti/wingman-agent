package agents

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// An external agent that does not start leaves no agent behind for the host
// to close: a nil *acp.Agent in the interface would panic it.
func TestAnExternalAgentThatFailsToStartIsNil(t *testing.T) {
	ws, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := newExternal(t.Context(), ws, code.AgentDef{Name: "broken"})
	if err == nil {
		t.Fatal("an agent without a command started")
	}
	if a != nil {
		t.Fatalf("a failed start returned %#v", a)
	}
}
