package harness

import (
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func (a *Agent) pinTools() {
	if !a.PinTools {
		return
	}
	var tools []tool.Tool
	if a.Tools != nil {
		tools = slices.Clone(a.Tools())
	}
	a.queueMu.Lock()
	a.turnTools, a.toolsPinned = tools, true
	a.queueMu.Unlock()
}

// AvailableTools returns the active catalog with current host policy applied.
// With PinTools, catalog changes become visible on the next turn, while policy
// changes remain visible at every request boundary. Callbacks must not mutate
// tool definitions after publishing them.
func (a *Agent) AvailableTools() []tool.Tool {
	a.queueMu.Lock()
	tools, pinned := slices.Clone(a.turnTools), a.toolsPinned
	a.queueMu.Unlock()
	if !pinned && a.Tools != nil {
		tools = slices.Clone(a.Tools())
	}
	if a.FilterTools != nil {
		tools = a.FilterTools(tools)
	}
	return tools
}
