package server

import (
	"context"
	"net/http"
)

type modelFetcher interface {
	FetchModels(context.Context) error
}

func (b *backendRuntime) handleRefreshModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := b.agent.(modelFetcher); !ok {
		http.Error(w, "model discovery is managed by this agent", http.StatusMethodNotAllowed)
		return
	}
	if err := b.refreshModels(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Startup and retries share one request. Closing a picker does not cancel
// discovery for other sessions; workspace shutdown still cancels it promptly.
func (b *backendRuntime) refreshModels() error {
	_, err, _ := b.modelDiscovery.Do("models", func() (any, error) {
		if !b.beginOperation() {
			return nil, context.Canceled
		}
		defer b.operations.Done()
		err := b.agent.(modelFetcher).FetchModels(b.ctx)
		if b.ctx.Err() != nil {
			return nil, b.ctx.Err()
		}
		b.mu.Lock()
		views := make([]*sessionController, 0, len(b.sessions))
		for _, view := range b.sessions {
			views = append(views, view)
		}
		b.mu.Unlock()
		for _, view := range views {
			view.refreshSettings()
		}
		b.broadcast(Frame{Type: EvtModelChanged})
		return nil, err
	})
	return err
}
