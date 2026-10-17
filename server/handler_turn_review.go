package server

import (
	"context"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/go-chi/chi/v5"
)

type turnReviewer interface {
	TurnReviews(context.Context, string) ([]agent.TurnReview, error)
	UndoTurn(context.Context, string, string) error
}

func (b *backendRuntime) handleTurnReviews(w http.ResponseWriter, r *http.Request) {
	reviewer, ok := b.agent.(turnReviewer)
	if !ok {
		writeJSON(w, []agent.TurnReview{})
		return
	}
	reviews, err := reviewer.TurnReviews(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	turnID := chi.URLParam(r, "turnID")
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
	if err := reviewer.UndoTurn(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "turnID")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	b.flushFiles()
	b.broadcast(Frame{Type: EvtDiffsChanged})
	w.WriteHeader(http.StatusNoContent)
}
