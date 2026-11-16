package server

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// handleTerminalStatus reports whether a terminal runs a command besides its
// shell, so closing it can ask for confirmation.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	uri, err := url.PathUnescape(chi.URLParam(r, "terminal"))
	if err != nil {
		http.Error(w, "invalid terminal URI", http.StatusBadRequest)
		return
	}
	h := s.Project
	h.startTerminals()
	h.host.mu.Lock()
	terminal := h.terminalChannels[uri]
	h.host.mu.Unlock()
	if terminal == nil {
		http.Error(w, "terminal not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"busy": terminal.session.HasForegroundProcess()})
}
