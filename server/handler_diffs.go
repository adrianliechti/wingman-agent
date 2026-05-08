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
