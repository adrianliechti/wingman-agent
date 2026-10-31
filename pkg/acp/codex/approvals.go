package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

const (
	optionAllowOnce   acpsdk.PermissionOptionId = "allow-once"
	optionAllowAlways acpsdk.PermissionOptionId = "allow-always"
	optionRejectOnce  acpsdk.PermissionOptionId = "reject-once"
	optionCancel      acpsdk.PermissionOptionId = "cancel"

	optionExecpolicyAmendment     acpsdk.PermissionOptionId = "accept-execpolicy-amendment"
	optionAllowPermissionsTurn    acpsdk.PermissionOptionId = "allow-permissions-turn"
	optionAllowPermissionsSession acpsdk.PermissionOptionId = "allow-permissions-session"
	optionRejectPermissions       acpsdk.PermissionOptionId = "reject-permissions"
)

func permissionOptions() []acpsdk.PermissionOption {
	return []acpsdk.PermissionOption{
		{OptionId: optionAllowOnce, Name: "Allow Once", Kind: acpsdk.PermissionOptionKindAllowOnce},
		{OptionId: optionAllowAlways, Name: "Allow for Session", Kind: acpsdk.PermissionOptionKindAllowAlways},
		{OptionId: optionRejectOnce, Name: "Reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
	}
}

type approvalChoice struct {
	option   acpsdk.PermissionOption
	decision any
}

func permissionOption(id acpsdk.PermissionOptionId, name string, kind acpsdk.PermissionOptionKind, changes ...map[string]any) acpsdk.PermissionOption {
	option := acpsdk.PermissionOption{OptionId: id, Name: name, Kind: kind}
	if len(changes) > 0 {
		option.Meta = map[string]any{"permission": map[string]any{"version": 1, "changes": changes}}
	}
	return option
}

type approver struct {
	ctx       context.Context
	conn      acp.ClientConnection
	sessionID acpsdk.SessionId
	client    acpsdk.ClientCapabilities
}

func newApprover(ctx context.Context, conn acp.ClientConnection, sid acpsdk.SessionId, client acpsdk.ClientCapabilities) *approver {
	return &approver{
		ctx: ctx, conn: conn, sessionID: sid, client: client,
	}
}

func (a *approver) forRequest(requestCtx context.Context) (*approver, func()) {
	ctx, cancel := context.WithCancel(a.ctx)
	stop := context.AfterFunc(requestCtx, cancel)
	if requestCtx.Err() != nil {
		cancel()
	}
	return newApprover(ctx, a.conn, a.sessionID, a.client), func() {
		stop()
		cancel()
	}
}

func pendingToolCall(id string, kind acpsdk.ToolKind) acpsdk.ToolCallUpdate {
	status := acpsdk.ToolCallStatusPending
	return acpsdk.ToolCallUpdate{ToolCallId: acpsdk.ToolCallId(id), Kind: &kind, Status: &status}
}

func (a *approver) ask(tc acpsdk.ToolCallUpdate) (id acpsdk.PermissionOptionId, ok bool) {
	return a.askWithOptions(tc, permissionOptions())
}

func (a *approver) askWithOptions(tc acpsdk.ToolCallUpdate, options []acpsdk.PermissionOption) (id acpsdk.PermissionOptionId, ok bool) {
	resp, err := callClient(a.ctx, a.conn, func() (acpsdk.RequestPermissionResponse, error) {
		return a.conn.RequestPermission(a.ctx, acpsdk.RequestPermissionRequest{
			SessionId: a.sessionID, ToolCall: tc, Options: options,
		})
	})
	if err != nil || a.ctx.Err() != nil || resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil {
		return "", false
	}
	return resp.Outcome.Selected.OptionId, true
}

func (a *approver) handleExec(p execApprovalParams, toolName string) execApprovalResponse {
	toolCallID, title := p.ItemID, "Run command"
	if p.Kind == "writeStdin" {
		title, toolName = "Send input to terminal", "write_stdin"
	}
	// Callbacks for an already running command get their own card so the command's tool call keeps its state.
	separate := p.Kind == "writeStdin" || (p.ApprovalID != "" && p.ApprovalID != p.ItemID)
	if separate {
		approvalID := p.ApprovalID
		if approvalID == "" {
			approvalID = uuid.NewString()
		}
		toolCallID = "approval:" + approvalID
	}
	tc := pendingToolCall(toolCallID, acpsdk.ToolKindExecute)
	tc.Title = &title
	tc.Meta = toolNameMeta(toolName)
	if p.Reason != "" {
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(p.Reason))}
	}
	if p.Command != "" {
		command := stripShellPrefix(p.Command)
		if command == "" {
			command = p.Command
		}
		tc.RawInput = commandRawInput(command, p.Cwd)
	}

	response := execApprovalResponse{Decision: "cancel"}
	if choices := commandApprovalChoices(p); len(choices) > 0 {
		if id, ok := a.askWithOptions(tc, approvalOptions(choices)); ok {
			response.Decision = unmatchedCommandDecision(choices)
			for _, choice := range choices {
				if choice.option.OptionId == id {
					response.Decision = choice.decision
				}
			}
		}
	}
	if separate {
		status := acpsdk.ToolCallStatusCompleted
		if response.Decision == "decline" || response.Decision == "cancel" {
			status = acpsdk.ToolCallStatusFailed
		}
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 2*time.Second)
		defer cancel()
		_ = notifyClient(finishCtx, a.conn, a.sessionID, acpsdk.UpdateToolCall(tc.ToolCallId,
			acpsdk.WithUpdateStatus(status),
			acpsdk.WithUpdateRawOutput(map[string]any{"decision": response.Decision}),
		))
	}
	return response
}

func unmatchedCommandDecision(choices []approvalChoice) any {
	for _, choice := range choices {
		if choice.decision == "decline" {
			return "decline"
		}
	}
	return "cancel"
}

func (a *approver) handleFile(p fileApprovalParams) fileApprovalResponse {
	tc := pendingToolCall(p.ItemID, acpsdk.ToolKindEdit)
	tc.Title = new("Edit files")
	if p.Reason != "" {
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(p.Reason))}
	}

	choices := fileApprovalChoices(p)
	id, ok := a.askWithOptions(tc, approvalOptions(choices))
	if !ok {
		return fileApprovalResponse{Decision: "cancel"}
	}
	for _, choice := range choices {
		if choice.option.OptionId == id {
			return fileApprovalResponse{Decision: choice.decision.(string)}
		}
	}
	return fileApprovalResponse{Decision: "decline"}
}

func (a *approver) handlePermissions(p permissionsApprovalParams) permissionsApprovalResponse {
	tc := permissionsToolCall(p)

	options := []acpsdk.PermissionOption{
		permissionOption(optionAllowPermissionsSession, "Allow for Session", acpsdk.PermissionOptionKindAllowAlways, permissionGrantChanges(p.Permissions, "session")...),
		permissionOption(optionAllowPermissionsTurn, "Allow Once", acpsdk.PermissionOptionKindAllowOnce, permissionGrantChanges(p.Permissions, "turn")...),
		permissionOption(optionRejectPermissions, "Reject", acpsdk.PermissionOptionKindRejectOnce),
	}
	id, ok := a.askWithOptions(tc, options)
	if !ok {
		return rejectPermissionsResponse()
	}
	switch id {
	case optionAllowPermissionsSession, optionAllowAlways:
		return permissionsApprovalResponse{Permissions: grantedPermissions(p.Permissions), Scope: "session", StrictAutoReview: false}
	case optionAllowPermissionsTurn, optionAllowOnce:
		return permissionsApprovalResponse{Permissions: grantedPermissions(p.Permissions), Scope: "turn", StrictAutoReview: false}
	default:
		return rejectPermissionsResponse()
	}
}

func permissionsToolCall(p permissionsApprovalParams) acpsdk.ToolCallUpdate {
	tc := pendingToolCall(p.ItemID, acpsdk.ToolKindOther)
	tc.Title = new("Permissions request")
	tc.Meta = toolNameMeta("request_permissions")
	if content := formatRequestedPermissions(p.Permissions); content != "" {
		if p.Reason != "" {
			content = p.Reason + "\n\n" + content
		}
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(content))}
	} else if p.Reason != "" {
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(p.Reason))}
	}
	return tc
}

func approvalOptions(choices []approvalChoice) []acpsdk.PermissionOption {
	options := make([]acpsdk.PermissionOption, 0, len(choices))
	for _, choice := range choices {
		options = append(options, choice.option)
	}
	return options
}

type commandDecision struct {
	name       string
	execpolicy []string
	network    *networkPolicyAmendment
}

func commandApprovalChoices(p execApprovalParams) []approvalChoice {
	decisions := defaultCommandDecisions(p)
	if p.AvailableDecisions != nil {
		decisions = parseCommandDecisions(p.AvailableDecisions)
	}
	offersDecline := slices.ContainsFunc(decisions, func(d commandDecision) bool { return d.name == "decline" })
	choices := make([]approvalChoice, 0, len(decisions))
	networkIndex := 0
	for _, decision := range decisions {
		switch {
		case decision.execpolicy != nil:
			choices = append(choices, execpolicyChoice(decision.execpolicy))
		case decision.network != nil:
			choices = append(choices, networkPolicyChoice(networkIndex, *decision.network))
			networkIndex++
		case decision.name == "accept":
			choices = append(choices, approvalChoice{permissionOption(optionAllowOnce, "Allow Once", acpsdk.PermissionOptionKindAllowOnce), "accept"})
		case decision.name == "acceptForSession":
			choices = append(choices, sessionCommandChoice(p.NetworkApprovalContext))
		case decision.name == "decline":
			choices = append(choices, approvalChoice{permissionOption(optionRejectOnce, "Reject", acpsdk.PermissionOptionKindRejectOnce), "decline"})
		case decision.name == "cancel":
			name := "Reject"
			if offersDecline {
				name = "Reject and Stop"
			}
			choices = append(choices, approvalChoice{permissionOption(optionCancel, name, acpsdk.PermissionOptionKindRejectOnce), "cancel"})
		}
	}
	return choices
}

// Older app-servers omit availableDecisions.
func defaultCommandDecisions(p execApprovalParams) []commandDecision {
	decisions := []commandDecision{{name: "accept"}, {name: "acceptForSession"}}
	if len(p.ProposedExecpolicyAmendment) > 0 {
		decisions = append(decisions, commandDecision{execpolicy: p.ProposedExecpolicyAmendment})
	}
	for _, amendment := range p.ProposedNetworkPolicyAmendments {
		decisions = append(decisions, commandDecision{network: &amendment})
	}
	return append(decisions, commandDecision{name: "decline"})
}

func parseCommandDecisions(raw []json.RawMessage) []commandDecision {
	decisions := make([]commandDecision, 0, len(raw))
	for _, value := range raw {
		var name string
		if json.Unmarshal(value, &name) == nil {
			decisions = append(decisions, commandDecision{name: name})
			continue
		}
		var object struct {
			Execpolicy *struct {
				Amendment []string `json:"execpolicy_amendment"`
			} `json:"acceptWithExecpolicyAmendment"`
			Network *struct {
				Amendment networkPolicyAmendment `json:"network_policy_amendment"`
			} `json:"applyNetworkPolicyAmendment"`
		}
		if json.Unmarshal(value, &object) != nil {
			continue
		}
		switch {
		case object.Execpolicy != nil && len(object.Execpolicy.Amendment) > 0:
			decisions = append(decisions, commandDecision{execpolicy: object.Execpolicy.Amendment})
		case object.Network != nil && object.Network.Amendment.Host != "":
			decisions = append(decisions, commandDecision{network: &object.Network.Amendment})
		}
	}
	return decisions
}

func sessionCommandChoice(network *networkApprovalContext) approvalChoice {
	name := "Allow for Session"
	var changes []map[string]any
	if network != nil && network.Host != "" {
		name = "Allow Host for Session"
		matcher := map[string]any{"type": "host", "host": network.Host}
		if network.Protocol != "" {
			matcher["protocol"] = network.Protocol
		}
		changes = append(changes, map[string]any{
			"type": "grant", "operation": "grant",
			"description": fmt.Sprintf("Allow access to %s for this session", network.Host),
			"lifetime":    map[string]any{"scope": "session"},
			"targets":     []any{map[string]any{"type": "network", "matcher": matcher}},
		})
	}
	return approvalChoice{permissionOption(optionAllowAlways, name, acpsdk.PermissionOptionKindAllowAlways, changes...), "acceptForSession"}
}

func execpolicyChoice(amendment []string) approvalChoice {
	prefix := strings.Join(amendment, " ")
	label := "Allow and Remember Command Pattern"
	if prefix != "" && !strings.ContainsAny(prefix, "\r\n") {
		label = fmt.Sprintf("Allow Commands Starting With `%s`", prefix)
	}
	return approvalChoice{
		permissionOption(optionExecpolicyAmendment, label, acpsdk.PermissionOptionKindAllowAlways, map[string]any{
			"type": "policy_rule", "operation": "add", "ruleBehavior": "allow",
			"description": "Allow commands starting with " + prefix,
			"targets":     []any{map[string]any{"type": "command", "matcher": map[string]any{"type": "argv_prefix", "argv": amendment}}},
		}),
		map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": amendment}},
	}
}

func networkPolicyChoice(index int, amendment networkPolicyAmendment) approvalChoice {
	kind := acpsdk.PermissionOptionKindAllowAlways
	verb := "Allow"
	if amendment.Action == "deny" {
		kind = acpsdk.PermissionOptionKindRejectAlways
		verb = "Block"
	}
	id := acpsdk.PermissionOptionId(fmt.Sprintf("apply-network-policy-amendment:%d", index))
	return approvalChoice{
		permissionOption(id, fmt.Sprintf("%s %s in the Future", verb, amendment.Host), kind, map[string]any{
			"type": "policy_rule", "operation": "add", "ruleBehavior": amendment.Action,
			"description": fmt.Sprintf("%s access to %s", verb, amendment.Host),
			"targets":     []any{map[string]any{"type": "network", "matcher": map[string]any{"type": "host", "host": amendment.Host}}},
		}),
		map[string]any{"applyNetworkPolicyAmendment": map[string]any{"network_policy_amendment": amendment}},
	}
}

func fileApprovalChoices(p fileApprovalParams) []approvalChoice {
	name := "Allow for Session"
	var changes []map[string]any
	if p.GrantRoot != "" {
		name = "Allow Root for Session"
		changes = append(changes, map[string]any{
			"type": "grant", "operation": "grant",
			"description": fmt.Sprintf("Allow writes under %s for this session", p.GrantRoot),
			"lifetime":    map[string]any{"scope": "session"},
			"targets":     []any{map[string]any{"type": "filesystem", "access": []any{"write"}, "matcher": map[string]any{"type": "directory", "path": p.GrantRoot}}},
		})
	}
	return []approvalChoice{
		{permissionOption(optionAllowOnce, "Allow Once", acpsdk.PermissionOptionKindAllowOnce), "accept"},
		{permissionOption(optionAllowAlways, name, acpsdk.PermissionOptionKindAllowAlways, changes...), "acceptForSession"},
		{permissionOption(optionRejectOnce, "Reject", acpsdk.PermissionOptionKindRejectOnce), "decline"},
	}
}

func rejectPermissionsResponse() permissionsApprovalResponse {
	return permissionsApprovalResponse{Permissions: map[string]any{}, Scope: "turn", StrictAutoReview: true}
}

func grantedPermissions(requested map[string]any) map[string]any {
	granted := map[string]any{}
	for _, key := range []string{"network", "fileSystem"} {
		if value, ok := requested[key]; ok && value != nil {
			granted[key] = value
		}
	}
	return granted
}

func permissionGrantChanges(permissions map[string]any, scope string) []map[string]any {
	var changes []map[string]any
	lifetime := map[string]any{"scope": scope}
	suffix := " for this turn"
	if scope == "session" {
		suffix = " for this session"
	}
	if network, _ := permissions["network"].(map[string]any); network != nil {
		if enabled, ok := network["enabled"].(bool); ok {
			change := map[string]any{
				"type": "grant", "operation": "grant", "description": "Allow network access" + suffix,
				"lifetime": lifetime, "targets": []any{map[string]any{"type": "network", "matcher": map[string]any{"type": "any"}}},
			}
			if !enabled {
				change["type"] = "policy_rule"
				change["operation"] = "add"
				change["ruleBehavior"] = "deny"
				change["description"] = "Deny network access" + suffix
			}
			changes = append(changes, change)
		}
	}
	if fs, _ := permissions["fileSystem"].(map[string]any); fs != nil {
		for _, access := range []string{"read", "write"} {
			for _, path := range stringSlice(fs[access]) {
				changes = append(changes, map[string]any{
					"type": "grant", "operation": "grant", "description": fmt.Sprintf("Allow %s access to %s%s", access, path, suffix),
					"lifetime": lifetime,
					"targets":  []any{map[string]any{"type": "filesystem", "access": []any{access}, "matcher": map[string]any{"type": "exact_path", "path": path}}},
				})
			}
		}
	}
	return changes
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func formatRequestedPermissions(permissions map[string]any) string {
	var lines []string
	if network, _ := permissions["network"].(map[string]any); network != nil {
		if enabled, ok := network["enabled"].(bool); ok {
			lines = append(lines, fmt.Sprintf("Network Access: %t", enabled))
		}
	}
	if fs, _ := permissions["fileSystem"].(map[string]any); fs != nil {
		if paths := stringSlice(fs["read"]); len(paths) > 0 {
			lines = append(lines, "File System Read Access: "+strings.Join(paths, ", "))
		}
		if paths := stringSlice(fs["write"]); len(paths) > 0 {
			lines = append(lines, "File System Write Access: "+strings.Join(paths, ", "))
		}
		if entries, ok := fs["entries"]; ok {
			if raw, err := json.Marshal(entries); err == nil && string(raw) != "[]" && string(raw) != "null" {
				lines = append(lines, "File System Entries: "+string(raw))
			}
		}
	}
	return strings.Join(lines, "\n\n")
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
	if !isMessageOnlyElicitation(p) {
		return elicitationResponse{Action: "decline", Content: nil, Meta: nil}
	}
	title := p.Message
	if title == "" {
		title = "Approve MCP request"
	}
	if p.ServerName != "" {
		title = fmt.Sprintf("%s: %s", p.ServerName, title)
	}
	tc := pendingToolCall("mcp-elicitation:"+uuid.NewString(), acpsdk.ToolKindOther)
	tc.Title = &title
	if p.Message != "" {
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(p.Message))}
	}

	response := elicitationResponse{Action: "cancel"}
	if id, ok := a.ask(tc); ok {
		switch id {
		case optionAllowOnce, optionAllowAlways:
			response.Action = "accept"
		default:
			response.Action = "decline"
		}
	}
	// Synthetic permission cards have no backend completion event; close them even after cancellation.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 2*time.Second)
	defer cancel()
	_ = notifyClient(finishCtx, a.conn, a.sessionID, acpsdk.UpdateToolCall(tc.ToolCallId,
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted),
		acpsdk.WithUpdateRawOutput(map[string]any{"action": response.Action}),
	))
	return response
}

func isMessageOnlyElicitation(p elicitationParams) bool {
	if p.Mode != "form" {
		return false
	}
	schema := strings.TrimSpace(string(p.RequestedSchema))
	if schema == "" || schema == "null" {
		return true
	}
	var requested map[string]json.RawMessage
	if json.Unmarshal(p.RequestedSchema, &requested) != nil {
		return false
	}
	var schemaType string
	if json.Unmarshal(requested["type"], &schemaType) != nil || schemaType != "object" {
		return false
	}
	propertiesRaw, ok := requested["properties"]
	if !ok || strings.TrimSpace(string(propertiesRaw)) == "null" {
		return false
	}
	var properties map[string]json.RawMessage
	return json.Unmarshal(propertiesRaw, &properties) == nil && len(properties) == 0
}

func (a *approver) elicitationMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta)+1)
	maps.Copy(out, meta)
	// acp-go-sdk v0.13.5 does not yet expose the request's top-level session
	// scope. Preserve it in metadata until the generated type catches up.
	out["sessionId"] = string(a.sessionID)
	return out
}

func (a *approver) handleFormElicitation(p elicitationParams) elicitationResponse {
	var schema acpsdk.UnstableElicitationSchema
	if len(p.RequestedSchema) > 0 {
		if err := json.Unmarshal(p.RequestedSchema, &schema); err != nil {
			return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
		}
	}
	if schema.Type == "" {
		schema.Type = acpsdk.UnstableElicitationSchemaTypeObject
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}

	resp, err := callClient(a.ctx, a.conn, func() (acpsdk.UnstableCreateElicitationResponse, error) {
		return a.conn.UnstableCreateElicitation(a.ctx, acpsdk.UnstableCreateElicitationRequest{
			Form: &acpsdk.UnstableCreateElicitationForm{
				Meta:            a.elicitationMeta(p.Meta),
				Message:         p.Message,
				Mode:            "form",
				RequestedSchema: schema,
			},
		})
	})
	if err != nil || a.ctx.Err() != nil || resp.Cancel != nil {
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
	if p.ElicitationID == "" || p.URL == "" {
		return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
	}
	// MCP servers can reuse their own IDs across sessions. ACP requires each
	// outstanding URL interaction to have a unique ID on the client connection.
	id := acpsdk.UnstableElicitationId(uuid.NewString())
	resp, err := callClient(a.ctx, a.conn, func() (acpsdk.UnstableCreateElicitationResponse, error) {
		return a.conn.UnstableCreateElicitation(a.ctx, acpsdk.UnstableCreateElicitationRequest{
			Url: &acpsdk.UnstableCreateElicitationUrl{
				Meta:          a.elicitationMeta(p.Meta),
				ElicitationId: id,
				Message:       p.Message,
				Mode:          "url",
				Url:           p.URL,
			},
		})
	})
	if err != nil || a.ctx.Err() != nil || resp.Cancel != nil {
		return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
	}
	if resp.Decline != nil {
		return elicitationResponse{Action: "decline", Content: nil, Meta: resp.Decline.Meta}
	}
	if resp.Accept != nil {
		// Consent to open a URL does not mean its external workflow has finished.
		// serverRequest/resolved only closes the app-server request, so it cannot
		// justify an ACP elicitation/complete notification. Completion is optional.
		return elicitationResponse{Action: "accept", Meta: resp.Accept.Meta}
	}
	return elicitationResponse{Action: "cancel", Content: nil, Meta: nil}
}
