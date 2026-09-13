package code

import (
	"context"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// Backend RPCs may request UI approval themselves, so they must never run on
// the input loop. Results apply only to the session activation that started them.
func (a *App) runSessionOperation(label string, work func(context.Context, string) error, done func()) {
	if a.operationPending {
		a.showToast("A session update is already running", theme.Default.Yellow)
		return
	}
	id, epoch := a.sessionID, a.sessionEpoch
	ctx, cancel := context.WithTimeout(code.WithSessionID(a.ctx, id), 30*time.Second)
	a.operationPending = true
	a.operationCancel = cancel
	a.invalidate()
	go func() {
		err := work(ctx, id)
		cancel()
		a.post(func() {
			a.operationPending = false
			a.operationCancel = nil
			if a.sessionID != id || a.sessionEpoch != epoch {
				return
			}
			if err != nil {
				a.showToast(label+": "+err.Error(), theme.Default.Red)
			} else if done != nil {
				done()
			}
			a.invalidate()
		})
	}()
}
