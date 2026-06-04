package codex

import (
	"context"
	"fmt"

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

type approver struct {
	ctx       context.Context
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId
}

func newApprover(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId) *approver {
	return &approver{ctx: ctx, conn: conn, sessionID: sid}
}

func pendingToolCall(id string, kind acp.ToolKind) acp.ToolCallUpdate {
	status := acp.ToolCallStatusPending
	return acp.ToolCallUpdate{ToolCallId: acp.ToolCallId(id), Kind: &kind, Status: &status}
}

func (a *approver) ask(tc acp.ToolCallUpdate) (id acp.PermissionOptionId, ok bool) {
	resp, err := a.conn.RequestPermission(a.ctx, acp.RequestPermissionRequest{
		SessionId: a.sessionID,
		ToolCall:  tc,
		Options:   permissionOptions(),
	})
	if err != nil || resp.Outcome.Cancelled != nil {
		return "", false
	}
	return resp.Outcome.Selected.OptionId, true
}

func (a *approver) handleExec(p execApprovalParams) execApprovalResponse {
	tc := pendingToolCall(p.ItemID, acp.ToolKindExecute)
	if p.Reason != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(p.Reason))}
	}
	if p.Command != "" {
		tc.RawInput = map[string]any{"command": stripShellPrefix(p.Command), "cwd": p.Cwd}
	}

	id, ok := a.ask(tc)
	if !ok {
		return execApprovalResponse{Decision: "cancel"}
	}
	switch id {
	case optionAllowOnce:
		return execApprovalResponse{Decision: "accept"}
	case optionAllowAlways:
		return execApprovalResponse{Decision: "acceptForSession"}
	default:
		return execApprovalResponse{Decision: "decline"}
	}
}

func (a *approver) handleFile(p fileApprovalParams) fileApprovalResponse {
	tc := pendingToolCall(p.ItemID, acp.ToolKindEdit)
	if p.Reason != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(p.Reason))}
	}

	id, ok := a.ask(tc)
	if !ok {
		return fileApprovalResponse{Decision: "cancel"}
	}
	switch id {
	case optionAllowOnce:
		return fileApprovalResponse{Decision: "accept"}
	case optionAllowAlways:
		return fileApprovalResponse{Decision: "acceptForSession"}
	default:
		return fileApprovalResponse{Decision: "cancel"}
	}
}

func (a *approver) handleElicitation(p elicitationParams) elicitationResponse {
	if p.Mode != "form" {
		return elicitationResponse{Action: "decline"}
	}
	title := p.Message
	if title == "" {
		title = "Approve MCP request"
	}
	if p.ServerName != "" {
		title = fmt.Sprintf("%s: %s", p.ServerName, title)
	}
	tc := pendingToolCall(fmt.Sprintf("mcp-elicitation:%s", p.ServerName), acp.ToolKindOther)
	tc.Title = &title
	if p.Message != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(p.Message))}
	}

	id, ok := a.ask(tc)
	if !ok {
		return elicitationResponse{Action: "cancel"}
	}
	switch id {
	case optionAllowOnce, optionAllowAlways:
		return elicitationResponse{Action: "accept"}
	default:
		return elicitationResponse{Action: "decline"}
	}
}
