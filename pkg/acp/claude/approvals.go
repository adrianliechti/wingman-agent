package claude

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/acp-go-sdk"
)

const (
	optionAllowOnce   acp.PermissionOptionId = "allow-once"
	optionAllowAlways acp.PermissionOptionId = "allow-always"
	optionRejectOnce  acp.PermissionOptionId = "reject-once"
)

// toolCallTracker decides, for a given tool_use id, whether the next sighting
// should emit a tool_call (the first sighting) or a tool_call_update (any
// later one). The CLI can report a tool_use to us via two independent paths
// that race on different goroutines — the permission control_request (handled
// by approver.handle) and the streamed assistant message (handled by
// emitAssistant) — so the decision and the network write that acts on it run
// while holding the tracker's lock. That serializes the two paths for a given
// id and guarantees the tool_call always reaches the wire before any
// tool_call_update referencing it, regardless of which path is first.
type toolCallTracker struct {
	mu      sync.Mutex
	emitted map[string]bool
}

func newToolCallTracker() *toolCallTracker {
	return &toolCallTracker{emitted: map[string]bool{}}
}

func (t *toolCallTracker) emit(id string, start, refine func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.emitted[id] {
		return refine()
	}
	t.emitted[id] = true
	return start()
}

func permissionOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: optionAllowOnce, Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: optionAllowAlways, Name: "Allow for Session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: optionRejectOnce, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
}

type approver struct {
	ctx     context.Context
	conn    *acp.AgentSideConnection
	sid     acp.SessionId
	out     *streamWriter
	cwd     string
	emitted *toolCallTracker
}

func pendingToolCall(id string, kind acp.ToolKind) acp.ToolCallUpdate {
	status := acp.ToolCallStatusPending
	return acp.ToolCallUpdate{ToolCallId: acp.ToolCallId(id), Kind: &kind, Status: &status}
}

func (a *approver) handle(req controlRequest) {
	if req.Request.Subtype != "can_use_tool" {
		a.respondError(req.RequestID, "unsupported control request: "+req.Request.Subtype)
		return
	}

	name := req.Request.ToolName
	id := req.Request.ToolUseID

	// The CLI can invoke can_use_tool before the assistant message's tool_use
	// block streams to us, so a permission request can reference a tool_call
	// the client has never seen. Emit it now if no one has yet, so the client
	// can always associate the prompt below with a known tool call.
	if id != "" && shouldEmitToolCall(name) && a.emitted != nil {
		_ = a.emitted.emit(id, func() error {
			return a.emitToolCallStart(id, name, req.Request.Input)
		}, func() error {
			return nil
		})
	}

	tc := pendingToolCall(id, toolKindFor(name))
	title := name
	if title == "" {
		title = "Tool call"
	}
	tc.Title = &title
	if len(req.Request.Input) > 0 {
		var input map[string]any
		if json.Unmarshal(req.Request.Input, &input) == nil {
			tc.RawInput = input
		}
	}
	if req.Request.Description != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(req.Request.Description))}
	}

	resp, err := a.conn.RequestPermission(a.ctx, acp.RequestPermissionRequest{
		SessionId: a.sid,
		ToolCall:  tc,
		Options:   permissionOptions(),
	})
	allow := err == nil && resp.Outcome.Cancelled == nil &&
		(resp.Outcome.Selected.OptionId == optionAllowOnce || resp.Outcome.Selected.OptionId == optionAllowAlways)
	a.respond(req.RequestID, allow, req.Request.Input)
}

func (a *approver) emitToolCallStart(id, name string, rawInput json.RawMessage) error {
	return a.conn.SessionUpdate(a.ctx, acp.SessionNotification{
		SessionId: a.sid,
		Update:    toolCallStartUpdate(id, name, rawInput, a.cwd, acp.ToolCallStatusPending),
	})
}

func (a *approver) respond(requestID string, allow bool, input json.RawMessage) {
	var body map[string]any
	if allow {
		updated := json.RawMessage("{}")
		if len(input) > 0 {
			updated = input
		}
		body = map[string]any{"behavior": "allow", "updatedInput": updated}
	} else {
		body = map[string]any{"behavior": "deny", "message": "Rejected by user"}
	}
	_ = a.out.writeJSON(controlResponse{
		Type:     "control_response",
		Response: controlResponseBody{Subtype: "success", RequestID: requestID, Response: body},
	})
}

func (a *approver) respondError(requestID, msg string) {
	_ = a.out.writeJSON(controlResponse{
		Type:     "control_response",
		Response: controlResponseBody{Subtype: "error", RequestID: requestID, Error: msg},
	})
}
