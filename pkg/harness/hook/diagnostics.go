package hook

import (
	"context"
	"errors"
	"log/slog"
)

// ReportError reports a hook failure without changing its event's control-flow
// policy. Index is the zero-based registration index within the event/source;
// source is empty for in-process callbacks. Arguments and hook output are not
// logged. Cancellation of the containing operation is not a hook failure.
func ReportError(ctx context.Context, event, source string, index int, err error) {
	if err == nil || ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))) {
		return
	}
	runtime := RuntimeFromContext(ctx)
	slog.WarnContext(ctx, "hook failed",
		"event", event, "source", source, "handler_index", index,
		"session_id", runtime.SessionID, "turn_id", runtime.TurnID,
		"agent_id", runtime.AgentID, "error", err)
}
