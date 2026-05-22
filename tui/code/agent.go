package code

import (
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// showAgentPicker is a no-op placeholder in the TUI for now — the TUI
// runs against the in-process wingman backend exclusively. External
// ACP agents are selectable from the Web UI. The slash command still
// resolves so users get a clear message about where to switch.
func (a *App) showAgentPicker() {
	t := theme.Default
	a.switchToChat()
	defs := code.LoadAgents()
	msg := "Agent switching is available in the Web UI. The TUI runs against the in-process wingman backend."
	if len(defs) > 0 {
		var names []string
		for _, d := range defs {
			names = append(names, d.Name)
		}
		msg = fmt.Sprintf("%s (configured external agents: %v)", msg, names)
	}
	fmt.Fprint(a.chatView, a.formatNotice(msg, t.Yellow))
}
