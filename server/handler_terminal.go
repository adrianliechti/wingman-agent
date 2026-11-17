package server

import (
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/go-chi/chi/v5"
)

// handleTerminals lists the terminal channels of the window's project; the
// root lists those of every project the host runs.
func (s *Server) handleTerminals(w http.ResponseWriter, r *http.Request) {
	p := s.Project
	p.startTerminals()
	p.host.mu.Lock()
	uris := slices.Sorted(maps.Keys(p.terminalChannels))
	p.host.mu.Unlock()
	if uris == nil {
		uris = []string{}
	}
	writeJSON(w, uris)
}

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
