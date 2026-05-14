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
	diffs, err := s.workspace.Diffs()
	if err != nil {
		// ErrClosed means the manager was torn down while this poll was in
		// flight. Silently return empty; the next poll lands on a fresh one.
		if !errors.Is(err, rewind.ErrClosed) {
			fmt.Fprintf(os.Stderr, "diffs: %v\n", err)
		}
		writeJSON(w, []DiffEntry{})
		return
	}

	result := []DiffEntry{}
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

	writeJSON(w, result)
}

func (s *Server) handleDiffRevert(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.resolveWorkspacePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	relPath := r.URL.Query().Get("path")

	diffs, err := s.workspace.Diffs()
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

	s.broadcast(Frame{Type: EvtDiffsChanged})
	s.broadcast(Frame{Type: EvtFilesChanged})

	w.WriteHeader(http.StatusNoContent)
}
