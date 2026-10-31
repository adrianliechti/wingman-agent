package server

import (
	"context"
	"net/http"
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
				writeJSON(w, review)
				return
			}
		}
		http.Error(w, "turn not found", http.StatusNotFound)
		return
	}
	// Listing cards does not transfer every before/after file version.
	session := r.PathValue("id")
	listing := make([]turnReviewListing, len(reviews))
	for i, review := range reviews {
		listing[i] = turnReviewListing{TurnReview: review}
		listing[i].Added, listing[i].Removed = b.turnLineStats(session, review)
		for j := range review.Files {
			review.Files[j].Before, review.Files[j].After = "", ""
		}
	}
	writeJSON(w, listing)
}

type turnReviewListing struct {
	harness.TurnReview
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// turnLineStats caches finished turns, whose recorded edits no longer change.
func (b *backendRuntime) turnLineStats(session string, review harness.TurnReview) (added, removed int) {
	key := session + "/" + review.ID
	if cached, ok := b.reviewStats.Load(key); ok {
		stats := cached.([2]int)
		return stats[0], stats[1]
	}
	added, removed = netLineStats(review.Files)
	if review.Outcome != "running" {
		b.reviewStats.Store(key, [2]int{added, removed})
	}
	return added, removed
}

// netLineStats compares each file as the agent first found it with its last
// version, so a later edit that reverts an earlier one nets out.
func netLineStats(files []tool.FileChange) (added, removed int) {
	first, last := map[string]string{}, map[string]string{}
	for _, file := range files {
		if _, ok := first[file.Path]; !ok {
			first[file.Path] = file.Before
		}
		last[file.Path] = file.After
	}
	for path, before := range first {
		a, r := diff.LineStats(before, last[path], time.Second)
		added, removed = added+a, removed+r
	}
	return added, removed
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
