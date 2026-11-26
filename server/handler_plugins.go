package server

import "net/http"

type PluginEntry struct {
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	Description     string `json:"description,omitempty"`
	Scope           string `json:"scope"`
	Skills          int    `json:"skills"`
	MCPServers      int    `json:"mcp_servers"`
	LanguageServers int    `json:"language_servers"`
	Hooks           bool   `json:"hooks"`
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	plugins := s.Project.workspace.Plugins
	result := make([]PluginEntry, 0, len(plugins))
	for _, p := range plugins {
		scope := "project"
		if p.Personal {
			scope = "personal"
		}
		result = append(result, PluginEntry{
			Name:            p.Name,
			Version:         p.Manifest.Version,
			Description:     p.Manifest.Description,
			Scope:           scope,
			Skills:          len(p.Skills),
			MCPServers:      len(p.Servers),
			LanguageServers: len(p.LanguageServers),
			Hooks:           p.Hooks.RuleCount() > 0,
		})
	}
	writeJSON(w, result)
}
