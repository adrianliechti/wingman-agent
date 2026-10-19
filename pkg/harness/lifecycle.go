package harness

import (
	"context"
	"errors"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

// ErrClosed means the session has been closed and cannot start more work.
var ErrClosed = errors.New("agent session is closed")

// Go starts workers whose lifetime belongs to the agent, independent of any
// turn. Close cancels their context and waits for them before releasing resources.
// Workers must honor cancellation and must not call Close on this agent.
func (a *Agent) Go(workers ...func(context.Context)) error {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if a.backgroundCtx == nil {
		a.backgroundCtx, a.cancelBackground = context.WithCancel(context.Background())
	}
	ctx := a.backgroundCtx
	for _, worker := range workers {
		a.background.Go(func() { worker(ctx) })
	}
	return nil
}

// beginOperation owns the session through preparation and finalization. A
// canceled operation keeps its reservation until its caller has unwound.
// The owner calls the returned release function exactly once.
func (a *Agent) beginOperation(ctx context.Context) (context.Context, func(), error) {
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
	a.operationDone = make(chan struct{})
	return ctx, func() {
		cancel(nil)
		a.queueMu.Lock()
		a.cancel = nil
		a.turnID = ""
		a.running, a.finishing = false, false
		a.turnTools, a.toolsPinned = nil, false
		if a.operationDone != nil {
			close(a.operationDone)
			a.operationDone = nil
		}
		a.queueMu.Unlock()
	}, nil
}

// detachStream ends Close's wait for setup without releasing the turn's
// reservation. Streaming stays owned by its consumer through finalization.
func (a *Agent) detachStream() {
	a.queueMu.Lock()
	close(a.operationDone)
	a.operationDone = nil
	a.queueMu.Unlock()
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
	ctx, release, err := a.beginOperation(ctx)
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

// Close rejects new operations, cancels active work, joins workers, releases
// owned resources, and invokes SessionEnd hooks once. Synchronous operations
// and turn setup settle before cleanup; Close does not wait for a Send stream
// consumer. Setup callbacks must not call Close on this agent.
// The caller supplies a cleanup context independent of the active turn.
func (a *Agent) Close(ctx context.Context, reason string) error {
	a.queueMu.Lock()
	if a.closed {
		a.queueMu.Unlock()
		return nil
	}
	a.closed = true
	cancel := a.cancel
	done := a.operationDone
	cancelBackground := a.cancelBackground
	a.queueMu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
	if cancelBackground != nil {
		cancelBackground()
	}
	if done != nil {
		<-done
	}
	a.background.Wait()
	var err error
	if a.Resources != nil {
		err = a.Resources.Close()
	}
	if a.Config == nil {
		return err
	}
	ctx = hook.WithRuntime(ctx, a.hookRuntime(ctx))
	for _, h := range a.Hooks.SessionEnd {
		h(ctx, reason)
	}
	return err
}
