package server

import (
	"encoding/json"
	"net/http"
)

// modeStringFor reads a session's current mode. PlanMode lives on the
// session's agent (its instructions builder reads it directly), so the
// server doesn't need to track a parallel copy.
func modeStringFor(sess *Session) string {
	if sess != nil && sess.Agent != nil && sess.Agent.PlanMode {
		return "plan"
	}
	return "agent"
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	writeJSON(w, map[string]string{"mode": modeStringFor(sess)})
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Mode != "agent" && body.Mode != "plan" {
		http.Error(w, "mode must be \"agent\" or \"plan\"", http.StatusBadRequest)
		return
	}

	sess := s.sessionFromRequest(r)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	sess.Agent.PlanMode = body.Mode == "plan"

	writeJSON(w, map[string]string{"mode": modeStringFor(sess)})
}
