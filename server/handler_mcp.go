package server

import "net/http"

type MCPServerEntry struct {
	Name      string         `json:"name"`
	Transport string         `json:"transport,omitempty"`
	Target    string         `json:"target,omitempty"`
	Scope     string         `json:"scope,omitempty"`
	State     string         `json:"state"`
	Error     string         `json:"error,omitempty"`
	Tools     []MCPToolEntry `json:"tools"`
}

type MCPToolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	servers := s.Project.workspace.MCPServers()
	result := make([]MCPServerEntry, 0, len(servers))
	for _, server := range servers {
		entry := MCPServerEntry{
			Name:      server.Name,
			Transport: server.Transport,
			Target:    server.Target,
			Scope:     server.Scope,
			State:     string(server.State),
			Error:     server.Error,
			Tools:     make([]MCPToolEntry, 0, len(server.Tools)),
		}
		for _, tool := range server.Tools {
			entry.Tools = append(entry.Tools, MCPToolEntry{Name: tool.Name, Description: tool.Description})
		}
		result = append(result, entry)
	}
	writeJSON(w, result)
}
