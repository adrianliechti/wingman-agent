package server

import (
	"encoding/json"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/code/wingman"
)

// Plan mode is a wingman-only affordance — ACP backends have their own
// internal mode handling. We expose it via the [*wingman.Agent] type
// assertion; non-wingman backends report mode="agent" and reject sets
// with 405.

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session")
	if id == "" {
		writeJSON(w, map[string]string{"mode": "agent"})
		return
	}
	mode := "agent"
	if wa, ok := s.activeAgent().(*wingman.Agent); ok && wa.PlanMode(id) {
		mode = "plan"
	}
	writeJSON(w, map[string]string{"mode": mode})
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
	wa, ok := s.activeAgent().(*wingman.Agent)
	if !ok {
		http.Error(w, "plan mode is only available with the wingman backend", http.StatusMethodNotAllowed)
		return
	}
	wa.SetPlanMode(id, body.Mode == "plan")
	writeJSON(w, map[string]string{"mode": body.Mode})
}
