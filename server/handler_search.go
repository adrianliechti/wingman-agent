package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const maxWorkspaceSearches = 8

// handleWorkspaceSearch streams matching files as NDJSON; aborting the request
// cancels the search.
func (s *Server) handleWorkspaceSearch(w http.ResponseWriter, r *http.Request) {
	var request workspaceSearchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid search request", http.StatusBadRequest)
		return
	}
	if _, err := newWorkspaceSearchConfig(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.searches.Add(1) > maxWorkspaceSearches {
		s.searches.Add(-1)
		http.Error(w, "too many searches", http.StatusTooManyRequests)
		return
	}
	defer s.searches.Add(-1)
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	encoder, flush := json.NewEncoder(w), http.NewResponseController(w)
	// A client that stops reading would otherwise hold its search slot forever.
	_ = flush.SetWriteDeadline(deadline)
	defer flush.SetWriteDeadline(time.Time{})
	var summary workspaceSearchEvent
	err := s.Project.searchWorkspace(ctx, request, func(event workspaceSearchEvent) error {
		if event.Type == "done" {
			summary = event
			return nil
		}
		if err := encoder.Encode(event); err != nil {
			return err
		}
		return flush.Flush()
	})
	if err != nil {
		_ = encoder.Encode(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	_ = encoder.Encode(map[string]any{"type": "done", "files": summary.Files, "matches": summary.Matches, "truncated": summary.Truncated})
}
