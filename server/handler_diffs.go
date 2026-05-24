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
	rel, ok := s.workspaceRel(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	// Diff.Path is slash-separated; rel may use OS separators.
	canonical := filepath.ToSlash(rel)

	diffs, err := s.workspace.Diffs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var match *rewind.FileDiff
	for i := range diffs {
		if diffs[i].Path == canonical {
			match = &diffs[i]
			break
		}
	}
	if match == nil {
		http.Error(w, "no diff for path", http.StatusNotFound)
		return
	}

	root := s.workspace.Root

	switch match.Status {
	case rewind.StatusAdded:
		if err := root.Remove(rel); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case rewind.StatusModified, rewind.StatusDeleted:
		if dir := filepath.Dir(rel); dir != "." && dir != "" {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := root.WriteFile(rel, []byte(match.Original), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	s.broadcast(Frame{Type: EvtDiffsChanged})
	s.broadcast(Frame{Type: EvtFilesChanged})

	w.WriteHeader(http.StatusNoContent)
}
