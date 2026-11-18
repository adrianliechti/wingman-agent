package code

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestWarnShowsAgentWarningInVisibleSession(t *testing.T) {
	a, _ := newStreamTestApp(nil)
	warning := "Loaded only 10 of 20 bytes from /workspace/AGENTS.md"
	a.Warn("other", "a warning for a session that is not shown")
	a.Warn("session", warning)
	a.Notice("session", code.Notice{Severity: "info", Message: "Settings loaded"})
	a.Notice("session", code.Notice{Severity: "error", Message: "Hook stopped"})
	for len(a.queue) > 0 {
		(<-a.queue)()
	}

	chat := func() string { return ansi.Strip(strings.Join(a.chatViewLines(100), "\n")) }
	if out := chat(); strings.Count(out, "Warning: "+warning) != 1 || strings.Contains(out, "not shown") {
		t.Fatalf("chat = %q", out)
	}
	if out := chat(); !strings.Contains(out, "Settings loaded") || strings.Contains(out, "Warning: Settings loaded") || !strings.Contains(out, "Error: Hook stopped") {
		t.Fatalf("notice severities in chat = %q", out)
	}
	// The notice is not part of the message history; a rebuild must keep it.
	a.rebuildChat()
	if out := chat(); strings.Count(out, "Warning: "+warning) != 1 {
		t.Fatalf("chat after rebuild = %q", out)
	}
}
