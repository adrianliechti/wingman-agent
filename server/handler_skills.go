package server

import "net/http"

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Project.skillCatalogue())
}
