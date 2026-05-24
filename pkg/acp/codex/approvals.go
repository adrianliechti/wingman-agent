package codex

import (
	"context"

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

// approver bridges codex's approval RPC requests to ACP's session/request_permission.
// It's instantiated per turn so cancellation can propagate via the turn context.
type approver struct {
	ctx       context.Context
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId
}

func newApprover(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId) *approver {
	return &approver{ctx: ctx, conn: conn, sessionID: sid}
}

func (a *approver) handleExec(p execApprovalParams) execApprovalResponse {
	tc := acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(p.ItemID),
	}
	kind := acp.ToolKindExecute
	tc.Kind = &kind
	status := acp.ToolCallStatusPending
	tc.Status = &status
	if p.Reason != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(p.Reason))}
	}
	if p.Command != "" {
		tc.RawInput = map[string]any{"command": stripShellPrefix(p.Command), "cwd": p.Cwd}
	}

	resp, err := a.conn.RequestPermission(a.ctx, acp.RequestPermissionRequest{
		SessionId: a.sessionID,
		ToolCall:  tc,
		Options:   permissionOptions(),
	})
	if err != nil || resp.Outcome.Cancelled != nil {
		return execApprovalResponse{Decision: "cancel"}
	}
	switch resp.Outcome.Selected.OptionId {
	case optionAllowOnce:
		return execApprovalResponse{Decision: "accept"}
	case optionAllowAlways:
		return execApprovalResponse{Decision: "acceptForSession"}
	default:
		return execApprovalResponse{Decision: "decline"}
	}
}

func (a *approver) handleFile(p fileApprovalParams) fileApprovalResponse {
	tc := acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(p.ItemID),
	}
	kind := acp.ToolKindEdit
	tc.Kind = &kind
	status := acp.ToolCallStatusPending
	tc.Status = &status
	if p.Reason != "" {
		tc.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(p.Reason))}
	}

	resp, err := a.conn.RequestPermission(a.ctx, acp.RequestPermissionRequest{
		SessionId: a.sessionID,
		ToolCall:  tc,
		Options:   permissionOptions(),
	})
	if err != nil || resp.Outcome.Cancelled != nil {
		return fileApprovalResponse{Decision: "cancel"}
	}
	switch resp.Outcome.Selected.OptionId {
	case optionAllowOnce:
		return fileApprovalResponse{Decision: "accept"}
	case optionAllowAlways:
		return fileApprovalResponse{Decision: "acceptForSession"}
	default:
		return fileApprovalResponse{Decision: "cancel"}
	}
}
