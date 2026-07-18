package tool

import "context"

type progressSinkKey struct{}
type progressCallKey struct{}
type usageSinkKey struct{}

// WithProgressSink installs a UI callback that receives transient status text
// from running tool calls, keyed by tool-call ID.
func WithProgressSink(ctx context.Context, fn func(callID, text string)) context.Context {
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
	sink, _ := ctx.Value(progressSinkKey{}).(func(callID, text string))
	callID, _ := ctx.Value(progressCallKey{}).(string)

	if sink == nil || callID == "" {
		return nil
	}

	return func(text string) { sink(callID, text) }
}

type UsageDelta struct {
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
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
