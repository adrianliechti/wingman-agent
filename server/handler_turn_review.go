package server

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/diff"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type turnReviewer interface {
	TurnReviews(context.Context, string) ([]harness.TurnReview, error)
	UndoTurn(context.Context, string, string) error
}

func handleTurnReviews(c *sessionController, w http.ResponseWriter, r *http.Request) {
	reviewer, ok := c.backend.agent.(turnReviewer)
	if !ok {
		writeJSON(w, []harness.TurnReview{})
		return
	}
	reviews, err := reviewer.TurnReviews(r.Context(), c.ref.SessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	turnID := r.PathValue("turnID")
	if turnID != "" {
		for _, review := range reviews {
			if review.ID == turnID {
				// One net change per file, matching the listing's rows.
				review.Files = netFileChanges(review.Files)
				writeJSON(w, review)
				return
			}
		}
		http.Error(w, "turn not found", http.StatusNotFound)
		return
	}
	listing := make([]turnReviewListing, len(reviews))
	for i, review := range reviews {
		listing[i] = newTurnReviewListing(review)
	}
	writeJSON(w, listing)
}

// turnReviewListing summarizes each file's net change instead of sending
// every before/after version.
type turnReviewListing struct {
	harness.TurnReview
	Files []turnFileStats `json:"files"`
}

type turnFileStats struct {
	Path         string               `json:"path"`
	BeforeExists bool                 `json:"beforeExists"`
	AfterExists  bool                 `json:"afterExists"`
	Added        int                  `json:"added"`
	Removed      int                  `json:"removed"`
	BeforeRef    *tool.FileContentRef `json:"beforeRef,omitempty"`
	AfterRef     *tool.FileContentRef `json:"afterRef,omitempty"`
	MovedFrom    string               `json:"movedFrom,omitempty"`
}

func newTurnReviewListing(review harness.TurnReview) turnReviewListing {
	net := netFileChanges(review.Files)
	files := make([]turnFileStats, len(net))
	for i, file := range net {
		added, removed := diff.LineStats(file.Before, file.After, time.Second)
		files[i] = turnFileStats{
			Path: file.Path, MovedFrom: file.MovedFrom,
			BeforeExists: file.BeforeExists, AfterExists: file.AfterExists,
			BeforeRef: file.BeforeRef, AfterRef: file.AfterRef,
			Added: added, Removed: removed,
		}
	}
	return turnReviewListing{TurnReview: review, Files: files}
}

// netFileChanges folds the edits like undo does, keeping forward-slash paths for the UI.
func netFileChanges(changes []tool.FileChange) []tool.FileChange {
	net, _ := tool.NetFileChanges(changes)
	moved := make(map[string]*tool.FileContentRef)
	for _, file := range net {
		if file.MovedFrom != "" && file.AfterRef != nil {
			moved[filepath.Clean(filepath.FromSlash(file.MovedFrom))] = file.AfterRef
		}
	}
	visible := net[:0]
	for _, file := range net {
		if file.BeforeRef != nil && file.Unchanged() {
			continue
		}
		if !file.AfterExists && (!file.BeforeExists || file.BeforeRef != nil && file.BeforeRef.Equal(moved[file.Path])) {
			continue
		}
		file.Path = filepath.ToSlash(file.Path)
		file.MovedFrom = filepath.ToSlash(file.MovedFrom)
		visible = append(visible, file)
	}
	return visible
}

func handleTurnUndo(c *sessionController, w http.ResponseWriter, r *http.Request) {
	reviewer, ok := c.backend.agent.(turnReviewer)
	if !ok {
		http.Error(w, "turn undo is unavailable for this agent", http.StatusNotImplemented)
		return
	}
	if err := reviewer.UndoTurn(r.Context(), c.ref.SessionID, r.PathValue("turnID")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	c.backend.Project.flushFiles()
	c.backend.Project.refreshChangesets()
	c.refreshChanges()
	w.WriteHeader(http.StatusNoContent)
}
