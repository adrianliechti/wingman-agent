package server

import (
	"context"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
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
	for i := range reviews {
		for j := range reviews[i].Files {
			reviews[i].Files[j].Before, reviews[i].Files[j].After = "", ""
		}
	}
	writeJSON(w, reviews)
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
