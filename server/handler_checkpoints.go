package server

import (
	"net/http"
)

func (s *Server) handleCheckpoints(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil || sess.Agent.Rewind == nil {
		writeJSON(w, []CheckpointEntry{})
		return
	}

	checkpoints, err := sess.Agent.Rewind.List()
	if err != nil {
		writeJSON(w, []CheckpointEntry{})
		return
	}

	result := make([]CheckpointEntry, 0, len(checkpoints))
	for _, cp := range checkpoints {
		result = append(result, CheckpointEntry{
			Hash:    cp.Hash,
			Message: cp.Message,
			Time:    cp.Time.Format("2006-01-02 15:04:05"),
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

	sess := s.sessionFromRequest(r)
	if sess == nil || sess.Agent.Rewind == nil {
		http.Error(w, "rewind not available", http.StatusServiceUnavailable)
		return
	}

	if err := sess.Agent.Rewind.Restore(hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Working tree just changed; nudge this session's diffs view and the
	// shared file tree (other sessions get their own DiffsChanged on next
	// poll fingerprint flip).
	sess.sendMessage(DiffsChangedEvent{})
	s.broadcast(FilesChangedEvent{})

	w.WriteHeader(http.StatusNoContent)
}
