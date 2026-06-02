package server

import (
	"encoding/json"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// AgentEntry is the JSON shape returned by /api/agents: one entry per
// selectable backend (built-in wingman first, then any [code.AgentDef]
// loaded from ~/.wingman/agents.json).
type AgentEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	result := []AgentEntry{
		{ID: code.BuiltinAgentName, Name: "Wingman"},
	}
	for _, r := range s.availableAgents() {
		result = append(result, AgentEntry{ID: r.Name, Name: r.Name})
	}
	writeJSON(w, result)
}

func (s *Server) handleAgent(w http.ResponseWriter, _ *http.Request) {
	a := s.activeAgent()
	name := code.BuiltinAgentName
	canDelete := false
	if a != nil {
		name = a.Name()
		canDelete = supportsDelete(a)
	}
	writeJSON(w, map[string]any{"agent": name, "canDelete": canDelete})
}

// supportsDelete reports whether the active backend can delete sessions.
// Backends may expose an optional SupportsDelete method (the ACP backend gates
// on the server's advertised capability); those that don't are assumed capable
// (the built-in wingman agent always is).
func supportsDelete(a code.Agent) bool {
	if d, ok := a.(interface{ SupportsDelete() bool }); ok {
		return d.SupportsDelete()
	}
	return true
}

func (s *Server) handleSetAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	name := body.Agent
	if name == "" {
		name = code.BuiltinAgentName
	}

	current := ""
	if a := s.activeAgent(); a != nil {
		current = a.Name()
	}
	if current == name {
		writeJSON(w, map[string]string{"agent": name})
		return
	}

	next, err := s.constructBackend(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.swapAgent(next)

	// Tell connected UIs to refetch agent-dependent state.
	s.broadcast(Frame{Type: EvtAgentChanged})
	writeJSON(w, map[string]string{"agent": name})
}
