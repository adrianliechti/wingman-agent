package harness

import (
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// NewToolPresentation creates display metadata without changing the execution
// name or arguments stored on a tool call.
func NewToolPresentation(name, kind, args string, locations []ToolLocation) *ToolPresentation {
	p := tool.Present(name, kind, args, len(locations) > 0)
	locs := slices.Clone(locations)
	if len(locs) == 0 {
		locs = p.Locations
	}
	return &ToolPresentation{
		Title: p.Title, Kind: p.Kind, Args: p.Args, Hint: p.Hint, Locations: locs,
	}
}
