package code

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestMCPServerLinesShowStateOriginAndTools(t *testing.T) {
	servers := []code.MCPServer{
		{
			Name: "docs", Transport: "http", Target: "https://mcp.example.com/mcp", Scope: "global",
			State: mcp.StateConnected,
			Tools: []code.MCPTool{
				{Name: "search", Description: "Search the\ndocumentation"},
				{Name: "a_tool_name_far_longer_than_the_name_column_allows", Description: "Read \x1b[31mone page"},
			},
		},
		{
			Name: "local", Transport: "stdio", Target: "npx -y server", Scope: "project",
			State: mcp.StateUnapproved, Error: "MCP server local needs approval of mcp.json before it can connect",
		},
		{Name: "idle", State: mcp.StatePending},
	}

	lines := mcpServerLines(servers, 60)
	for _, line := range lines {
		if strings.Contains(ansi.Strip(line), "\x1b") {
			t.Fatalf("server text reached the terminal unsanitized: %q", line)
		}
	}

	text := ansi.Strip(strings.Join(lines, "\n"))
	for _, want := range []string{
		"● docs  connected · 2 tools",
		"http · global · https://mcp.example.com/mcp",
		"search  ",
		"Search the documentation",
		"Read one page",
		"! local  not approved",
		"stdio · project · npx -y server",
		"needs approval of mcp.json",
		"○ idle  not started",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "a_tool_name_far_longer_than_the_name_column_allows") {
		t.Errorf("long tool name was not truncated:\n%s", text)
	}
}

func TestMCPCommandOpensOverlayOnlyWithServers(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 12)
	agent := a.agent.(*uiTestAgent)

	if cmd := a.findBuiltin("/mcp"); cmd == nil || !cmd.Busy {
		t.Fatalf("/mcp = %+v, want a command usable during a turn", cmd)
	}

	a.showMCPServers()
	if a.overlay != nil {
		t.Fatal("overlay opened without servers")
	}
	if chat := ansi.Strip(strings.Join(a.chat, "\n")); !strings.Contains(chat, "wingman mcp add") {
		t.Fatalf("chat = %q", chat)
	}

	manager := mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{"docs": {Command: "docs-server"}}})
	agent.workspace = &code.Workspace{RootPath: t.TempDir(), MCP: manager}
	defer agent.workspace.Close()

	a.showMCPServers()
	if a.overlay == nil {
		t.Fatal("overlay did not open")
	}
	screen := ansi.Strip(strings.Join(a.overlay.Render(80, 12), "\n"))
	for _, want := range []string{"MCP servers · 0 of 1 connected", "○ docs  not started", "stdio · global · docs-server"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in:\n%s", want, screen)
		}
	}
	if !a.overlay.HandleKey(inline.KeyEvent{Key: inline.KeyEsc}) {
		t.Fatal("esc did not close the overlay")
	}
}
