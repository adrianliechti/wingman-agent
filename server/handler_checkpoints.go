package server

import (
	"net/http"
	"time"
)

func (s *Server) handleCheckpoints(w http.ResponseWriter, r *http.Request) {
	checkpoints, err := s.workspace.Checkpoints()
	if err != nil {
		writeJSON(w, []CheckpointEntry{})
		return
	}

	result := make([]CheckpointEntry, 0, len(checkpoints))
	for _, cp := range checkpoints {
		result = append(result, CheckpointEntry{
			Hash:    cp.Hash,
			Message: cp.Message,
			Time:    cp.Time.Format(time.RFC3339),
		})
	}

	writeJSON(w, result)
}

func (s *Server) handleCheckpointRestore(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		http.Error(w, "hash required", http.StatusBadRequest)
		return
	}

	if err := s.workspace.Restore(hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtDiffsChanged})
	s.broadcast(Frame{Type: EvtFilesChanged})

	w.WriteHeader(http.StatusNoContent)
}
