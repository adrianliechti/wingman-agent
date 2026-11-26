package code

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

type MCPServer struct {
	Name      string
	Transport string
	Target    string
	Scope     string

	State mcp.State
	Error string

	Tools []MCPTool
}

type MCPTool struct {
	Name        string
	Description string
}

// MCPServers lists every configured server with its connection state and tools.
func (w *Workspace) MCPServers() []MCPServer {
	w.mu.RLock()
	manager := w.MCP
	toolsByServer := maps.Clone(w.mcpToolsByServer)
	w.mu.RUnlock()
	if manager == nil {
		return nil
	}

	names := slices.Collect(maps.Keys(manager.Servers))
	for name := range manager.Sessions() {
		if _, ok := manager.Servers[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	servers := make([]MCPServer, 0, len(names))
	for _, name := range names {
		status := manager.Status(name)
		server := MCPServer{
			Name:  name,
			State: status.State,
			Error: status.Error,
		}
		if config, ok := manager.Servers[name]; ok {
			server.Transport = config.Kind()
			server.Target = config.Target()
			server.Scope = w.mcpScope(manager.TrustRequired[name])
		}
		for _, t := range toolsByServer[name] {
			server.Tools = append(server.Tools, MCPTool{
				Name:        strings.TrimPrefix(t.Name, name+"_"),
				Description: t.Description,
			})
		}
		servers = append(servers, server)
	}
	return servers
}

// SetMCPChangedHandler reports changes to what MCPServers returns.
func (w *Workspace) SetMCPChangedHandler(handler func()) {
	w.mu.Lock()
	w.mcpChanged = handler
	manager := w.MCP
	w.mu.Unlock()
	if manager != nil {
		manager.SetStatusChangedHandler(handler)
	}
}

func (w *Workspace) notifyMCPChanged() {
	w.mu.RLock()
	handler := w.mcpChanged
	w.mu.RUnlock()
	if handler != nil {
		handler()
	}
}

func projectMCPConfigPath(workDir string) string {
	return filepath.Join(workDir, "mcp.json")
}

// Only project and plugin servers carry a trust source; see loadMCP.
func (w *Workspace) mcpScope(source string) string {
	switch source {
	case "":
		return "global"
	case projectMCPConfigPath(w.RootPath):
		return "project"
	default:
		return "plugin"
	}
}
