package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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
	client    acp.ClientCapabilities

	mu          sync.Mutex
	pendingURLs map[acp.UnstableElicitationId]struct{}
}

func newApprover(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, client acp.ClientCapabilities) *approver {
	return &approver{
		ctx: ctx, conn: conn, sessionID: sid, client: client,
		pendingURLs: make(map[acp.UnstableElicitationId]struct{}),
	}
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
	if a.client.Elicitation != nil {
		switch {
		case p.Mode == "form" && a.client.Elicitation.Form != nil:
			return a.handleFormElicitation(p)
		case p.Mode == "url" && a.client.Elicitation.Url != nil:
			return a.handleURLElicitation(p)
		}
	}
	if p.Mode != "form" && p.Mode != "openai/form" {
		return elicitationResponse{Action: "decline", Content: nil, Meta: nil}
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

func (a *approver) elicitationMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for key, value := range meta {
		out[key] = value
	}
	// acp-go-sdk v0.13.5 does not yet expose the request's top-level session
	// scope. Preserve it in metadata until the generated type catches up.
	out["sessionId"] = string(a.sessionID)
	return out
}

func (a *approver) handleFormElicitation(p elicitationParams) elicitationResponse {
	var schema acp.UnstableElicitationSchema
	if len(p.RequestedSchema) > 0 {
		if err := json.Unmarshal(p.RequestedSchema, &schema); err != nil {
			return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
		}
	}
	if schema.Type == "" {
		schema.Type = acp.UnstableElicitationSchemaTypeObject
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}

	resp, err := a.conn.UnstableCreateElicitation(a.ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Meta:            a.elicitationMeta(p.Meta),
			Message:         p.Message,
			Mode:            "form",
			RequestedSchema: schema,
		},
	})
	if err != nil || resp.Cancel != nil {
		return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
	}
	if resp.Decline != nil {
		return elicitationResponse{Action: "decline", Content: nil, Meta: resp.Decline.Meta}
	}
	if resp.Accept != nil {
		return elicitationResponse{Action: "accept", Content: resp.Accept.Content, Meta: resp.Accept.Meta}
	}
	return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
}

func (a *approver) handleURLElicitation(p elicitationParams) elicitationResponse {
	id := acp.UnstableElicitationId(p.ElicitationID)
	if id == "" || p.URL == "" {
		return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
	}
	resp, err := a.conn.UnstableCreateElicitation(a.ctx, acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			Meta:          a.elicitationMeta(p.Meta),
			ElicitationId: id,
			Message:       p.Message,
			Mode:          "url",
			Url:           p.URL,
		},
	})
	if err != nil || resp.Cancel != nil {
		return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
	}
	if resp.Decline != nil {
		return elicitationResponse{Action: "decline", Content: nil, Meta: resp.Decline.Meta}
	}
	if resp.Accept != nil {
		a.mu.Lock()
		a.pendingURLs[id] = struct{}{}
		a.mu.Unlock()
		return elicitationResponse{Action: "accept", Content: resp.Accept.Content, Meta: resp.Accept.Meta}
	}
	return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
}

func (a *approver) handleNotification(method string) {
	if method != "serverRequest/resolved" {
		return
	}
	a.mu.Lock()
	ids := make([]acp.UnstableElicitationId, 0, len(a.pendingURLs))
	for id := range a.pendingURLs {
		ids = append(ids, id)
	}
	clear(a.pendingURLs)
	a.mu.Unlock()
	for _, id := range ids {
		_ = a.conn.UnstableCompleteElicitation(a.ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: id})
	}
}
