package tool

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type progressSinkKey struct{}
type progressCallKey struct{}
type usageSinkKey struct{}
type backgroundOriginKey struct{}
type userInputKey struct{}

// ErrUserInput is the cancellation cause of a context that ended because the user sent a message.
var ErrUserInput = errors.New("the user sent a new message")

// WithUserInput lets the tools of a round observe signal, which is cancelled when the user sends a message.
func WithUserInput(ctx, signal context.Context) context.Context {
	return context.WithValue(ctx, userInputKey{}, signal)
}

// UntilUserInput returns a context that also ends when the user sends a message, for tools that only wait.
func UntilUserInput(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	signal, ok := ctx.Value(userInputKey{}).(context.Context)
	if !ok {
		return ctx, func() { cancel(nil) }
	}
	stop := context.AfterFunc(signal, func() {
		if context.Cause(signal) == ErrUserInput {
			cancel(ErrUserInput)
		}
	})
	return ctx, func() { stop(); cancel(nil) }
}

// WithBackgroundOrigin marks ctx as belonging to a detached background agent
// run, so session-scoped state (e.g. file freshness) can tell its tool calls
// apart from the main agent's.
func WithBackgroundOrigin(ctx context.Context, identity ...string) context.Context {
	id := ""
	if len(identity) > 0 {
		id = identity[0]
	}
	if id == "" {
		id = uuid.NewString()
	}
	return context.WithValue(ctx, backgroundOriginKey{}, id)
}

func BackgroundOrigin(ctx context.Context) string {
	v, _ := ctx.Value(backgroundOriginKey{}).(string)
	return v
}

// WithProgressSink installs a UI callback that receives transient status text
// from running tool calls, keyed by tool-call ID. The reporting context carries
// session metadata installed after the sink itself.
func WithProgressSink(ctx context.Context, fn func(context.Context, string, string)) context.Context {
	return context.WithValue(ctx, progressSinkKey{}, fn)
}

// WithProgressCall tags ctx with the tool-call ID that subsequent Progress
// reports attribute to.
func WithProgressCall(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, progressCallKey{}, callID)
}

// Progress returns a reporter for transient status text from a running tool
// call, or nil when no sink is installed. Reported text is display-only and
// never reaches the model.
func Progress(ctx context.Context) func(text string) {
	sink, _ := ctx.Value(progressSinkKey{}).(func(context.Context, string, string))
	callID, _ := ctx.Value(progressCallKey{}).(string)

	if sink == nil || callID == "" {
		return nil
	}

	return func(text string) {
		if ctx.Err() == nil {
			sink(ctx, callID, text)
		}
	}
}

type UsageDelta struct {
	InputTokens  int64
	OutputTokens int64

	ReasoningTokens int64

	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// WithUsageSink installs a callback that credits model usage a tool incurred
// internally (e.g. a subagent run) to the session's accounting.
func WithUsageSink(ctx context.Context, fn func(UsageDelta)) context.Context {
	return context.WithValue(ctx, usageSinkKey{}, fn)
}

// ReportUsage reports internally incurred model usage; a no-op without a sink.
func ReportUsage(ctx context.Context, d UsageDelta) {
	if fn, ok := ctx.Value(usageSinkKey{}).(func(UsageDelta)); ok && fn != nil {
		fn(d)
	}
}
