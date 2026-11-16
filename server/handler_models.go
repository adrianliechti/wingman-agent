package server

import (
	"net/http"
)

func handleRefreshModels(b *backendRuntime, w http.ResponseWriter, r *http.Request) {
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
