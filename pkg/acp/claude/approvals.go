package claude

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

const (
	optionAllowOnce   acp.PermissionOptionId = "allow-once"
	optionAllowAlways acp.PermissionOptionId = "allow-always"
	optionRejectOnce  acp.PermissionOptionId = "reject-once"
)

func permissionOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: optionAllowOnce, Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: optionAllowAlways, Name: "Allow for Session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: optionRejectOnce, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// approver bridges the claude CLI's stdio control protocol (`can_use_tool`) to
// ACP's session/request_permission. The decision is written back as a
// control_response on the CLI's stdin via out.
type approver struct {
	ctx  context.Context
	conn *acp.AgentSideConnection
	sid  acp.SessionId
	out  *streamWriter
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

	tc := pendingToolCall(req.Request.ToolUseID, toolKindFor(req.Request.ToolName))
	title := req.Request.ToolName
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
