package server

import (
	"encoding/json"
	"net/http"
)

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

	id := r.URL.Query().Get("session")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	// Lazy-create — mirrors MsgSend so the user can pick a mode before
	// the first message creates the session on its own.
	sess := s.getOrCreateSession(id)

	sess.Agent.PlanMode = body.Mode == "plan"

	writeJSON(w, map[string]string{"mode": modeStringFor(sess)})
}
