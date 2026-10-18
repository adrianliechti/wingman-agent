package harness

import (
	"context"
	"errors"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

// ErrClosed means the session has been closed and cannot start more work.
var ErrClosed = errors.New("agent session is closed")

// beginOperation owns the session through preparation and finalization. A
// canceled operation keeps its reservation until its caller has unwound.
func (a *Agent) beginOperation(ctx context.Context, waitOnClose bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.closed {
		return nil, nil, ErrClosed
	}
	if a.running {
		return nil, nil, ErrTurnInProgress
	}
	ctx, cancel := context.WithCancelCause(ctx)
	a.running, a.finishing = true, true
	a.cancel = cancel
	var done chan struct{}
	if waitOnClose {
		done = make(chan struct{})
	}
	a.closeWait = done
	return ctx, sync.OnceFunc(func() {
		cancel(nil)
		a.queueMu.Lock()
		a.cancel = nil
		a.closeWait = nil
		a.turnID = ""
		a.running, a.finishing = false, false
		a.turnTools, a.toolsPinned = nil, false
		if done != nil {
			close(done)
		}
		a.queueMu.Unlock()
	}), nil
}

// ActiveTurnID identifies the Send operation through finalization. It is empty
// during host preparation, between turns, and during non-turn operations.
func (a *Agent) ActiveTurnID() string {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	return a.turnID
}

// RunExclusive runs a host operation, such as a history fork, with the same
// exclusion and cancellation as Send, Compact and UndoTurn. The callback must
// not start another operation or call Close on this agent. Include finalization
// in fn and honor its context: Close waits for synchronous operations to settle.
func (a *Agent) RunExclusive(ctx context.Context, fn func(context.Context) error) error {
	ctx, release, err := a.beginOperation(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// Cancel interrupts the active operation, including host preparation.
func (a *Agent) Cancel(cause error) {
	a.queueMu.Lock()
	cancel := a.cancel
	a.queueMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// Close rejects new operations, cancels active work, releases host resources,
// and invokes SessionEnd hooks once. Synchronous operations settle before
// cleanup; Close does not wait for a Send stream consumer.
// The caller supplies a cleanup context independent of the active turn.
func (a *Agent) Close(ctx context.Context, reason string) error {
	a.queueMu.Lock()
	if a.closed {
		a.queueMu.Unlock()
		return nil
	}
	a.closed = true
	cancel := a.cancel
	done := a.closeWait
	a.queueMu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
	if done != nil {
		<-done
	}
	if a.Config == nil {
		return nil
	}
	var err error
	if a.CloseSession != nil {
		err = a.CloseSession()
	}
	ctx = hook.WithRuntime(ctx, a.hookRuntime(ctx))
	for _, h := range a.Hooks.SessionEnd {
		h(ctx, reason)
	}
	return err
}
