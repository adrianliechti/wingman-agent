package code

import (
	"context"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type operationKind uint8

const (
	operationNone operationKind = iota
	operationSession
	operationShell
	operationDiagnostics
	operationCompact
)

// Backend RPCs may request UI approval themselves, so they must never run on
// the input loop. Results apply only to the session activation that started them.
func (a *App) runSessionOperation(label string, work func(context.Context, string) error, done func()) {
	if a.operation != operationNone {
		a.showToast("A session update is already running", theme.Default.Yellow)
		return
	}
	id, epoch := a.sessionID, a.sessionEpoch
	ctx, cancel := context.WithTimeout(code.WithSessionID(a.ctx, id), 30*time.Second)
	a.operation = operationSession
	a.operationCancel = cancel
	a.invalidate()
	go func() {
		err := work(ctx, id)
		cancel()
		a.post(func() {
			a.operation = operationNone
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
