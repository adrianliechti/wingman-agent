package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

func TestReportErrorCorrelatesFailuresAndIgnoresOperationCancellation(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx := WithRuntime(t.Context(), Runtime{SessionID: "session", TurnID: "turn", AgentID: "child"})
	ReportError(ctx, "Stop", "plugin review", 2, errors.New("unavailable"))
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"event": "Stop", "source": "plugin review", "handler_index": float64(2),
		"session_id": "session", "turn_id": "turn", "agent_id": "child", "error": "unavailable",
	} {
		if entry[key] != want {
			t.Errorf("%s = %v, want %v", key, entry[key], want)
		}
	}
	logs.Reset()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	ReportError(canceled, "Stop", "", 0, nil)
	ReportError(canceled, "Stop", "", 0, fmt.Errorf("hook: %w", context.Canceled))
	cause := errors.New("host canceled the operation")
	withCause, cancelCause := context.WithCancelCause(ctx)
	cancelCause(cause)
	ReportError(withCause, "Stop", "", 0, fmt.Errorf("hook: %w", cause))
	if logs.Len() != 0 {
		t.Fatalf("operation cancellation reported as hook failure: %s", &logs)
	}
	// A hook's own deadline is still a failure when the operation is active.
	ReportError(ctx, "Stop", "", 0, context.DeadlineExceeded)
	if logs.Len() == 0 {
		t.Fatal("hook deadline was hidden")
	}
}
