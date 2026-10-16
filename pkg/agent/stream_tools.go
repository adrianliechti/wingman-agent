package agent

import (
	"context"
	"errors"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type toolStopKey struct{}

// Each tool round owns its stop scope, so child-agent stops cannot cancel the
// parent's turn. Cancellation prevents queued calls from reaching executors.
func withToolStop(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, toolStopKey{}, cancel), func() { cancel(nil) }
}

func stopToolTurn(ctx context.Context, reason string) {
	if cancel, ok := ctx.Value(toolStopKey{}).(context.CancelCauseFunc); ok {
		cancel(hookStopError(reason))
	}
}

// Completed calls start while the response is still streaming. Results are
// collected in call order after the response, keeping them behind its items in
// provider history. Reads may overlap; mutations wait for earlier work and
// prevent later reads from overtaking them.
type streamToolRound struct {
	agent       *Agent
	ctx         context.Context
	tools       []tool.Tool
	calls       []*streamToolCall
	barrier     <-chan struct{}
	reads       []<-chan struct{}
	parallelism int
}

type streamToolCall struct {
	call    ToolCall
	done    chan struct{}
	outcome toolCallOutcome
}

func (r *streamToolRound) start(call ToolCall) {
	if len(r.calls) == 0 {
		r.agent.beginToolRound()
		r.parallelism = r.agent.MaxParallelTools
		if r.parallelism == 0 {
			r.parallelism = DefaultMaxParallelTools
		}
	}
	readOnly := r.agent.isReadOnly(call, r.tools)
	entry := &streamToolCall{call: call, done: make(chan struct{})}
	var wait []<-chan struct{}
	if r.barrier != nil {
		wait = append(wait, r.barrier)
	}
	if readOnly {
		if r.parallelism > 0 && len(r.reads) >= r.parallelism {
			wait = append(wait, r.reads[len(r.reads)-r.parallelism])
		}
		r.reads = append(r.reads, entry.done)
	} else {
		wait = append(wait, r.reads...)
		r.reads = nil
		r.barrier = entry.done
	}
	r.calls = append(r.calls, entry)
	go func() {
		defer close(entry.done)
		for _, done := range wait {
			<-done
		}
		entry.outcome = r.agent.runSingleToolCallOutcomeDeferred(r.ctx, call, r.tools)
	}()
}

func (r *streamToolRound) drain(yield func(Message, error) bool) error {
	var err error
	for _, entry := range r.calls {
		<-entry.done
		outcome := r.agent.commitDeferredToolOutcome(entry.call, entry.outcome)
		message := toolResultMessage(entry.call, outcome.result)
		if !outcome.recorded {
			if appendErr := r.agent.appendMessages(message); appendErr != nil {
				err = errors.Join(err, appendErr)
				continue
			}
		}
		if err == nil && !yield(message, nil) {
			err = errYieldStopped
		}
	}
	if stop, ok := errors.AsType[hookStopError](context.Cause(r.ctx)); ok {
		err = errors.Join(err, stop)
	}
	return err
}
