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

func (b *backendRuntime) handleTurnReviews(w http.ResponseWriter, r *http.Request) {
	reviewer, ok := b.agent.(turnReviewer)
	if !ok {
		writeJSON(w, []harness.TurnReview{})
		return
	}
	reviews, err := reviewer.TurnReviews(r.Context(), r.PathValue("id"))
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
	Path         string `json:"path"`
	BeforeExists bool   `json:"beforeExists"`
	AfterExists  bool   `json:"afterExists"`
	Added        int    `json:"added"`
	Removed      int    `json:"removed"`
}

func newTurnReviewListing(review harness.TurnReview) turnReviewListing {
	net := netFileChanges(review.Files)
	files := make([]turnFileStats, len(net))
	for i, file := range net {
		added, removed := diff.LineStats(file.Before, file.After, time.Second)
		files[i] = turnFileStats{Path: file.Path, BeforeExists: file.BeforeExists, AfterExists: file.AfterExists, Added: added, Removed: removed}
	}
	return turnReviewListing{TurnReview: review, Files: files}
}

// netFileChanges folds the edits like undo does, keeping forward-slash paths for the UI.
func netFileChanges(changes []tool.FileChange) []tool.FileChange {
	net, _ := tool.NetFileChanges(changes)
	for i := range net {
		net[i].Path = filepath.ToSlash(net[i].Path)
	}
	return net
}

func (b *backendRuntime) handleTurnUndo(w http.ResponseWriter, r *http.Request) {
	reviewer, ok := b.agent.(turnReviewer)
	if !ok {
		http.Error(w, "turn undo is unavailable for this agent", http.StatusNotImplemented)
		return
	}
	if err := reviewer.UndoTurn(r.Context(), r.PathValue("id"), r.PathValue("turnID")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	b.flushFiles()
	b.ahpHost().refreshChangesets()
	w.WriteHeader(http.StatusNoContent)
}
