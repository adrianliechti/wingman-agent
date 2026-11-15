package server

import (
	"context"
)

type modelFetcher interface {
	FetchModels(context.Context) error
}

// Startup and retries share one request. Closing a picker does not cancel
// discovery for other sessions; workspace shutdown still cancels it promptly.
func (b *backendRuntime) refreshModels() error {
	_, err, _ := b.modelDiscovery.Do("models", func() (any, error) {
		if !b.Project.beginOperation() {
			return nil, context.Canceled
		}
		defer b.Project.operations.Done()
		err := b.agent.(modelFetcher).FetchModels(b.Project.ctx)
		if b.Project.ctx.Err() != nil {
			return nil, b.Project.ctx.Err()
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
		if b.Project != nil {
			b.Project.host.publishAgents()
		}
		return nil, err
	})
	return err
}
