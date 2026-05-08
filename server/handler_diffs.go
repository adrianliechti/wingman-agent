package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/rewind"
)

func (s *Server) handleDiffs(w http.ResponseWriter, r *http.Request) {
	if s.agent.Rewind == nil {
		writeJSON(w, []DiffEntry{})
		return
	}

	diffs, err := s.agent.Rewind.DiffFromBaseline()
	if err != nil {
		// ErrClosed means the manager was torn down (RestartRewind on
		// session-new) while this poll was in flight. Silently return empty;
		// the next poll lands on the fresh manager.
		if !errors.Is(err, rewind.ErrClosed) {
			fmt.Fprintf(os.Stderr, "diffs: %v\n", err)
		}
		writeJSON(w, []DiffEntry{})
		return
	}

	var result []DiffEntry

	for _, d := range diffs {
		status := "modified"
		switch d.Status {
		case rewind.StatusAdded:
			status = "added"
		case rewind.StatusDeleted:
			status = "deleted"
		}

		result = append(result, DiffEntry{
			Path:     d.Path,
			Status:   status,
			Patch:    d.Patch,
			Original: d.Original,
			Modified: d.Modified,
			Language: extToLanguage[strings.ToLower(filepath.Ext(d.Path))],
		})
	}

	if result == nil {
		result = []DiffEntry{}
	}

	writeJSON(w, result)
}

// handleDiffRevert restores a single file to its baseline state. Reverting a
// modified or deleted file writes the baseline content back; reverting an
// added file removes it. Per-file scope is what makes this distinct from
// /api/checkpoints/{hash}/restore, which rolls back the whole working tree.
func (s *Server) handleDiffRevert(w http.ResponseWriter, r *http.Request) {
	if s.agent.Rewind == nil {
		http.Error(w, "rewind not available", http.StatusServiceUnavailable)
		return
	}

	abs, ok := s.resolveWorkspacePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	relPath := r.URL.Query().Get("path")

	diffs, err := s.agent.Rewind.DiffFromBaseline()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var match *rewind.FileDiff
	for i := range diffs {
		if diffs[i].Path == relPath {
			match = &diffs[i]
			break
		}
	}
	if match == nil {
		http.Error(w, "no diff for path", http.StatusNotFound)
		return
	}

	switch match.Status {
	case rewind.StatusAdded:
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case rewind.StatusModified, rewind.StatusDeleted:
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(abs, []byte(match.Original), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	s.sendMessage(DiffsChangedEvent{})
	s.sendMessage(FilesChangedEvent{})

	w.WriteHeader(http.StatusNoContent)
}
