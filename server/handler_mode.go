package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// Modes are backend-advertised via the code.Agent interface; this handler is
// backend-agnostic and the UI renders whatever the active backend returns.

type modeOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type modeState struct {
	Current string       `json:"current"`
	Modes   []modeOption `json:"modes"`
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	a := s.activeAgent()
	if a == nil {
		writeJSON(w, toModeState(nil, ""))
		return
	}
	id := r.URL.Query().Get("session")
	available, current := a.Modes(id)
	writeJSON(w, toModeState(available, current))
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	id := r.URL.Query().Get("session")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	agent := s.activeAgent()
	if agent == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	if err := agent.SetMode(r.Context(), id, body.Mode); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			http.Error(w, "this backend has no selectable modes", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	available, current := agent.Modes(id)
	writeJSON(w, toModeState(available, current))
}

func toModeState(available []code.Mode, current string) modeState {
	modes := make([]modeOption, 0, len(available))
	for _, m := range available {
		modes = append(modes, modeOption{ID: m.ID, Name: m.Name, Description: m.Description})
	}
	return modeState{Current: current, Modes: modes}
}
