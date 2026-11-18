package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/acpjson"
	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// remoteV2 uses the SDK's ordered notification worker and request barriers.
// A prompt response confirms insertion. A separate idle notification settles
// the Wingman turn, even when it arrives before the insertion response.
type remoteV2 struct {
	*sdk.Connection
	host  *Agent
	mu    sync.Mutex
	turns map[sdk.SessionId]chan remoteIdle
}
type remoteIdle struct {
	response sdk.PromptResponse
	err      error
}

func newRemoteV2(host *Agent, out io.Writer, in io.Reader) *remoteV2 {
	r := &remoteV2{host: host, turns: map[sdk.SessionId]chan remoteIdle{}}
	r.Connection = sdk.NewConnection(r.handle, out, in)
	return r
}

func (r *remoteV2) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	idle := make(chan remoteIdle, 1)
	r.mu.Lock()
	if r.turns[p.SessionId] != nil {
		r.mu.Unlock()
		return sdk.PromptResponse{}, errors.New("ACP prompt already pending")
	}
	r.turns[p.SessionId] = idle
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.turns, p.SessionId); r.mu.Unlock() }()
	key := harness.InputIDFromContext(ctx)
	if key == "" {
		key = uuid.NewString()
	} else if utf8.RuneCountInString(key) > 255 {
		key = uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
	}
	ack, err := sdk.SendRequest[struct{ MessageId *string }](r.Connection, ctx, "session/prompt", map[string]any{"sessionId": p.SessionId, "prompt": p.Prompt, "idempotencyKey": key})
	if err != nil {
		return sdk.PromptResponse{}, err
	}
	if ack.MessageId == nil {
		return sdk.PromptResponse{}, errors.New("missing ACP v2 prompt messageId")
	}
	// Associate a late user-message echo with the optimistic local message.
	if session := r.host.session(string(p.SessionId)); session != nil {
		session.mu.Lock()
		if session.v2Messages == nil {
			session.v2Messages = map[string]int{}
		}
		for i := len(session.messages) - 1; i >= 0; i-- {
			if session.messages[i].Role == harness.RoleUser {
				session.v2Messages["user_message/"+*ack.MessageId] = i
				break
			}
		}
		session.mu.Unlock()
	}
	select {
	case result := <-idle:
		return result.response, result.err
	case <-ctx.Done():
		return sdk.PromptResponse{}, ctx.Err()
	case <-r.Done():
		select {
		case result := <-idle:
			return result.response, result.err
		default:
			return sdk.PromptResponse{}, io.ErrUnexpectedEOF
		}
	}
}

type remoteSetup struct {
	SessionID         sdk.SessionId
	ConfigOptions     []json.RawMessage
	AvailableCommands []json.RawMessage
}

func remoteCommands(id sdk.SessionId, entries []json.RawMessage) sdk.SessionNotification {
	var commands []sdk.AvailableCommand
	for _, entry := range entries {
		var c struct {
			Type, Name  string
			Description *string
			Input       *struct{ Type, Hint string }
		}
		if json.Unmarshal(entry, &c) != nil || c.Type != "command" || c.Description == nil || c.Name == "" || strings.ContainsAny(c.Name, " \t\n\r") {
			continue
		}
		command := sdk.AvailableCommand{Name: c.Name, Description: *c.Description}
		if c.Input != nil && c.Input.Type == "text" {
			command.Input = &sdk.AvailableCommandInput{Unstructured: &sdk.UnstructuredCommandInput{Hint: c.Input.Hint}}
		}
		commands = append(commands, command)
	}
	return sdk.SessionNotification{SessionId: id, Update: acp.UpdateAvailableCommands(commands)}
}

func remoteOptions(entries []json.RawMessage) ([]sdk.SessionConfigOption, error) {
	var options []sdk.SessionConfigOption
	for _, raw := range entries {
		entry := acpjson.Object(raw)
		if entry["type"] != "select" {
			continue
		}
		entry["id"] = entry["configId"]
		delete(entry, "configId")
		values, _ := entry["options"].([]any)
		for _, value := range values {
			if group, ok := value.(map[string]any); ok && group["groupId"] != nil {
				group["group"] = group["groupId"]
				delete(group, "groupId")
			}
		}
		var option sdk.SessionConfigOption
		if err := json.Unmarshal(acpjson.Raw(entry), &option); err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, nil
}

func (r *remoteV2) setup(ctx context.Context, method string, id sdk.SessionId, p any) (sdk.NewSessionResponse, error) {
	result, err := sdk.SendRequest[remoteSetup](r.Connection, ctx, method, p)
	if err != nil {
		return sdk.NewSessionResponse{}, err
	}
	options, err := remoteOptions(result.ConfigOptions)
	if err != nil {
		return sdk.NewSessionResponse{}, err
	}
	if method == "session/new" {
		id = result.SessionID
	}
	err = r.host.SessionUpdate(ctx, remoteCommands(id, result.AvailableCommands))
	return sdk.NewSessionResponse{SessionId: id, ConfigOptions: options}, err
}

func (r *remoteV2) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	return r.setup(ctx, "session/new", "", map[string]any{"cwd": p.Cwd})
}
func (r *remoteV2) LoadSession(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	s, err := r.setup(ctx, "session/resume", p.SessionId, map[string]any{"sessionId": p.SessionId, "cwd": p.Cwd, "replayFrom": map[string]any{"type": "start"}})
	return sdk.LoadSessionResponse{ConfigOptions: s.ConfigOptions}, err
}
func (r *remoteV2) ResumeSession(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	s, err := r.setup(ctx, "session/resume", p.SessionId, map[string]any{"sessionId": p.SessionId, "cwd": p.Cwd})
	return sdk.ResumeSessionResponse{ConfigOptions: s.ConfigOptions}, err
}
func (r *remoteV2) ListSessions(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.SendRequest[sdk.ListSessionsResponse](r.Connection, ctx, "session/list", p)
}
func (r *remoteV2) CloseSession(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.SendRequest[sdk.CloseSessionResponse](r.Connection, ctx, "session/close", p)
}
func (r *remoteV2) DeleteSession(ctx context.Context, p sdk.DeleteSessionRequest) (sdk.DeleteSessionResponse, error) {
	return sdk.SendRequest[sdk.DeleteSessionResponse](r.Connection, ctx, "session/delete", p)
}
func (r *remoteV2) Cancel(ctx context.Context, p sdk.CancelNotification) error {
	return r.SendNotification(ctx, "session/cancel", p)
}
func (r *remoteV2) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if p.ValueId == nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
	}
	v := p.ValueId
	res, err := sdk.SendRequest[remoteSetup](r.Connection, ctx, "session/set_config_option", map[string]any{"type": "id", "sessionId": v.SessionId, "configId": v.ConfigId, "value": v.Value})
	if err != nil {
		return sdk.SetSessionConfigOptionResponse{}, err
	}
	options, err := remoteOptions(res.ConfigOptions)
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: options}, err
}

func (r *remoteV2) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	sess := r.host.session(string(p.SessionId))
	if sess == nil {
		return sdk.SetSessionModeResponse{}, sdk.NewInvalidParams(nil)
	}
	sess.mu.Lock()
	id := sess.modeOptID
	sess.mu.Unlock()
	response, err := r.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{SessionId: p.SessionId, ConfigId: sdk.SessionConfigId(id), Value: sdk.SessionConfigValueId(p.ModeId)}})
	if err == nil {
		r.host.refreshConfig(sess, response.ConfigOptions)
	}
	return sdk.SetSessionModeResponse{}, err
}

func (r *remoteV2) handle(ctx context.Context, method string, raw json.RawMessage) (any, *sdk.RequestError) {
	switch method {
	case "session/update":
		var p struct {
			SessionId sdk.SessionId
			Update    json.RawMessage
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, sdk.NewInvalidParams(err.Error())
		}
		if err := r.update(ctx, p.SessionId, p.Update); err != nil {
			return nil, sdk.NewInvalidParams(err.Error())
		}
		return nil, nil
	case "session/request_permission":
		var p struct {
			SessionId          sdk.SessionId
			Title, Description string
			Options            []sdk.PermissionOption
			Subject            struct {
				Type, Command, Cwd string
				ToolCallId         sdk.ToolCallId
				ToolCall           sdk.ToolCallUpdate
			}
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, sdk.NewInvalidParams(err.Error())
		}
		call := p.Subject.ToolCall
		switch p.Subject.Type {
		case "tool_call":
		case "command":
			call.ToolCallId = p.Subject.ToolCallId
			call.Kind = sdk.PatchSet(sdk.ToolKindExecute)
			call.RawInput = sdk.PatchSet(acpjson.Raw(map[string]any{"command": p.Subject.Command, "cwd": p.Subject.Cwd}))
		default:
			return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{Cancelled: &sdk.RequestPermissionOutcomeCancelled{}}}, nil
		}
		if p.Title != "" {
			call.Title = sdk.PatchSet(p.Title)
		}
		result, err := r.host.RequestPermission(ctx, sdk.RequestPermissionRequest{SessionId: p.SessionId, ToolCall: call, Options: p.Options})
		if err != nil {
			return nil, sdk.NewInternalError(err.Error())
		}
		return result, nil
	case "elicitation/create":
		var p sdk.CreateElicitationRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, sdk.NewInvalidParams(err.Error())
		}
		result, err := r.host.CreateElicitation(ctx, p)
		if err != nil {
			return nil, sdk.NewInternalError(err.Error())
		}
		return result, nil
	default:
		return nil, sdk.NewMethodNotFound(method)
	}
}

func (r *remoteV2) update(ctx context.Context, id sdk.SessionId, raw json.RawMessage) error {
	var u struct {
		SessionUpdate, State, MessageId string
		Content                         json.RawMessage
		Error                           *sdk.RequestError
		StopReason                      sdk.StopReason
		Usage                           *sdk.Usage
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return err
	}
	switch u.SessionUpdate {
	case "state_update":
		if u.State == "idle" {
			result := remoteIdle{response: sdk.PromptResponse{StopReason: u.StopReason, Usage: u.Usage}}
			if u.StopReason == "error" {
				if u.Error != nil {
					result.err = u.Error
				} else {
					result.err = errors.New("ACP agent reported a turn error")
				}
			}
			if result.response.StopReason == "" {
				result.response.StopReason = sdk.StopReasonEndTurn
			}
			r.mu.Lock()
			idle := r.turns[id]
			if idle != nil {
				select {
				case idle <- result:
				default:
				}
			}
			r.mu.Unlock()
		}
		return nil
	case "agent_message", "user_message", "agent_thought", "agent_message_chunk", "user_message_chunk", "agent_thought_chunk":
		var blocks []sdk.ContentBlock
		kind, chunk := strings.CutSuffix(u.SessionUpdate, "_chunk")
		if chunk {
			var block sdk.ContentBlock
			if err := json.Unmarshal(u.Content, &block); err != nil {
				return err
			}
			blocks = []sdk.ContentBlock{block}
		} else if u.Content != nil {
			if err := json.Unmarshal(u.Content, &blocks); err != nil {
				return err
			}
		} else {
			return nil
		} // An omitted content field is a metadata-only patch.
		r.message(id, u.MessageId, kind, blocks, !chunk)
		return nil
	case "config_option_update":
		var v remoteSetup
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		options, err := remoteOptions(v.ConfigOptions)
		if err != nil {
			return err
		}
		return r.host.SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: sdk.SessionUpdate{ConfigOptionUpdate: &sdk.SessionConfigOptionUpdate{SessionUpdate: u.SessionUpdate, ConfigOptions: options}}})
	case "available_commands_update":
		var update remoteSetup
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		return r.host.SessionUpdate(ctx, remoteCommands(id, update.AvailableCommands))
	case "tool_call_update", "session_info_update", "usage_update", "notice":
		// These entities share their ID, content, and patch fields with the SDK.
	case "plan_update":
		var plan struct {
			Plan struct{ Entries []sdk.PlanEntry }
		}
		if err := json.Unmarshal(raw, &plan); err != nil {
			return err
		}
		raw = acpjson.Raw(map[string]any{"sessionUpdate": "plan", "entries": plan.Plan.Entries})
	default:
		return nil
	}
	if u.SessionUpdate == "tool_call_update" {
		v := acpjson.Object(raw)
		if s := r.host.session(string(id)); s != nil {
			toolID, _ := v["toolCallId"].(string)
			s.toolCallsMu.Lock()
			_, seen := s.toolCalls[toolID]
			s.toolCallsMu.Unlock()
			if !seen {
				v["sessionUpdate"] = "tool_call"
				if v["title"] == nil {
					v["title"] = v["name"]
				}
			}
		}
		if content, ok := v["content"].([]any); ok {
			for i, item := range content {
				part, _ := item.(map[string]any)
				if part["type"] == "diff" {
					patch, _ := part["patch"].(map[string]any)
					data, _ := patch["text"].(string)
					content[i] = map[string]any{"type": "content", "content": sdk.TextBlock(data)}
				}
			}
		}
		if v["status"] == "cancelled" {
			v["status"] = "failed"
		}
		raw = acpjson.Raw(v)
	}
	var update sdk.SessionUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return fmt.Errorf("ACP update %s: %w", u.SessionUpdate, err)
	}
	return r.host.SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: update})
}

// Preserve the identity and position of v2 message entities even when chunks
// for different messages interleave or the agent sends a replacement snapshot.
func (r *remoteV2) message(sid sdk.SessionId, id, kind string, blocks []sdk.ContentBlock, replace bool) {
	s := r.host.session(string(sid))
	if s == nil {
		return
	}
	role := harness.RoleAssistant
	if kind == "user_message" {
		role = harness.RoleUser
	}
	var incoming []harness.Content
	for _, b := range blocks {
		if kind == "agent_thought" {
			if text := blockText(b); text != "" {
				incoming = append(incoming, harness.Content{Reasoning: &harness.Reasoning{ID: id, Summary: text}})
			}
		} else if c, ok := messageBlockContent(b); ok {
			if c.Text != "" {
				c.TextID = id
			}
			incoming = append(incoming, c)
		}
	}
	key := kind + "/" + id
	s.mu.Lock()
	t := s.inflight
	if t != nil && t.ignoreUserUpdates && role == harness.RoleUser {
		s.mu.Unlock()
		return
	}
	if idx, ok := s.v2Messages[key]; ok {
		applyMessageContent(&s.messages[idx], incoming, replace)
		s.mu.Unlock()
		r.host.notifySessionChange(string(sid))
		return
	}
	if t == nil {
		if s.v2Messages == nil {
			s.v2Messages = map[string]int{}
		}
		s.v2Messages[key] = len(s.messages)
		msg := harness.Message{Role: role}
		if role == harness.RoleUser {
			msg.InputID = id
		}
		applyMessageContent(&msg, incoming, replace)
		s.messages = append(s.messages, msg)
		s.mu.Unlock()
		r.host.notifySessionChange(string(sid))
		return
	}
	s.mu.Unlock()
	t.updateMu.Lock()
	defer t.updateMu.Unlock()
	if t.ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	if t.v2Messages == nil {
		t.v2Messages = map[string]int{}
	}
	idx, seen := t.v2Messages[key]
	if !seen {
		idx = len(t.emitted)
		t.v2Messages[key] = idx
		msg := harness.Message{Role: role}
		if role == harness.RoleUser {
			msg.InputID = id
		}
		t.emitted = append(t.emitted, msg)
	}
	applyMessageContent(&t.emitted[idx], incoming, replace)
	t.lastContentKey = "v2-message"
	t.mu.Unlock()
	if len(incoming) > 0 {
		t.send(harness.Message{Role: role, Content: incoming})
	}
}

func applyMessageContent(msg *harness.Message, incoming []harness.Content, replace bool) {
	if replace {
		msg.Content = nil
	}
	for _, c := range harness.CloneContent(incoming) {
		n := len(msg.Content)
		if n == 0 || !mergeACPContent(&msg.Content[n-1], c) {
			msg.Content = append(msg.Content, c)
		}
	}
}
