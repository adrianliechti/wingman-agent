package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) showMCPServers() {
	workspace := a.agent.Workspace()
	if workspace == nil || len(workspace.MCPServers()) == 0 {
		a.appendChat(cellNotice("No MCP servers configured. Add one with 'wingman mcp add'.", theme.Default.Yellow, a.width()))
		return
	}

	a.showInfo(func(width int) (string, []string) {
		servers := workspace.MCPServers()
		connected := 0
		for _, server := range servers {
			if server.State == mcp.StateConnected {
				connected++
			}
		}
		return fmt.Sprintf("MCP servers · %d of %d connected", connected, len(servers)), mcpServerLines(servers, width)
	})
}

func mcpStateLabel(state mcp.State) (glyph, label string, color ansi.Color) {
	t := theme.Default

	switch state {
	case mcp.StateConnected:
		return "●", "connected", t.Green
	case mcp.StateConnecting:
		return "◌", "connecting", t.Cyan
	case mcp.StateFailed:
		return "✕", "failed", t.Red
	case mcp.StateUnapproved:
		return "!", "not approved", t.Yellow
	case mcp.StateLoginRequired:
		return "!", "needs login", t.Yellow
	case mcp.StateDisconnected:
		return "○", "disconnected", t.Red
	default:
		return "○", "not started", t.BrBlack
	}
}

const mcpToolNameWidth = 32

func mcpServerLines(servers []code.MCPServer, width int) []string {
	var lines []string

	for _, server := range servers {
		if len(lines) > 0 {
			lines = append(lines, "")
		}

		glyph, label, color := mcpStateLabel(server.State)
		head := colored(color, glyph) + " " + bold(markdown.Sanitize(server.Name)) + "  " + colored(color, label)
		if count := len(server.Tools); count == 1 {
			head += dim(" · 1 tool")
		} else if count > 1 {
			head += dim(fmt.Sprintf(" · %d tools", count))
		}
		lines = append(lines, cellIndent+head)

		var origin []string
		for _, part := range []string{server.Transport, server.Scope, server.Target} {
			if part != "" {
				origin = append(origin, markdown.Sanitize(part))
			}
		}
		if len(origin) > 0 {
			lines = append(lines, cellIndent+"  "+dim(strings.Join(origin, " · ")))
		}

		for raw := range strings.SplitSeq(strings.TrimSpace(markdown.Sanitize(server.Error)), "\n") {
			if raw == "" {
				continue
			}
			for _, line := range ansi.Wrap(colored(color, raw), max(width-len(cellIndent)-3, 10)) {
				lines = append(lines, cellIndent+"  "+line)
			}
		}

		if len(server.Tools) == 0 {
			continue
		}

		nameWidth := 0
		for _, tool := range server.Tools {
			nameWidth = max(nameWidth, ansi.Width(tool.Name))
		}
		nameWidth = min(nameWidth, mcpToolNameWidth)

		lines = append(lines, "")
		for _, tool := range server.Tools {
			name := ansi.Pad(ansi.Truncate(markdown.Sanitize(tool.Name), nameWidth, "…"), nameWidth)
			description := strings.Join(strings.Fields(markdown.Sanitize(tool.Description)), " ")
			lines = append(lines, cellIndent+"  "+name+"  "+dim(description))
		}
	}

	return lines
}
