package server

import (
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"net/http"
)

func (s *Server) handleTerminalShells(w http.ResponseWriter, _ *http.Request) {
	shells := terminal.Shells()
	if shells == nil {
		shells = []terminal.Shell{}
	}
	writeJSON(w, shells)
}
