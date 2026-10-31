package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

func object(value any) map[string]any {
	result := map[string]any{}
	decodeObject(value, &result)
	return result
}

func decodeObject(value, target any) {
	data, _ := json.Marshal(value)
	_ = json.Unmarshal(data, target)
}

func configFromObject(value any) []acp.SessionConfigOption {
	var options []acp.SessionConfigOption
	decodeObject(value, &options)
	return options
}

func v2Initialize(response acp.InitializeResponse) map[string]any {
	caps := response.AgentCapabilities
	prompt, mcp := map[string]any{}, map[string]any{"stdio": map[string]any{}}
	if caps.PromptCapabilities.Image {
		prompt["image"] = map[string]any{}
	}
	if caps.PromptCapabilities.EmbeddedContext {
		prompt["embeddedContext"] = map[string]any{}
	}
	if caps.McpCapabilities.Http {
		mcp["http"] = map[string]any{}
	}
	session := map[string]any{"prompt": prompt, "mcp": mcp}
	if caps.SessionCapabilities.Fork != nil {
		session["fork"] = map[string]any{}
	}
	if caps.SessionCapabilities.Delete != nil {
		session["delete"] = map[string]any{}
	}
	if caps.SessionCapabilities.AdditionalDirectories != nil {
		session["additionalDirectories"] = map[string]any{}
	}
	auth := []any{}
	for _, method := range response.AuthMethods {
		entry := object(method)
		entry["methodId"] = entry["id"]
		delete(entry, "id")
		if entry["type"] == nil {
			entry["type"] = "agent"
		}
		auth = append(auth, entry)
	}
	return map[string]any{"protocolVersion": 2, "info": response.AgentInfo, "capabilities": map[string]any{"session": session}, "authMethods": auth}
}

func v1Setup(raw json.RawMessage, caps acp.McpCapabilities) (map[string]any, error) {
	var p map[string]any
	if json.Unmarshal(raw, &p) != nil || p == nil {
		return nil, acp.NewInvalidParams(nil)
	}
	if p["systemPrompt"] != nil {
		return nil, acp.NewInvalidParams(map[string]any{"reason": "systemPrompt is not advertised"})
	}
	servers, ok := p["mcpServers"].([]any)
	if p["mcpServers"] != nil && !ok {
		return nil, acp.NewInvalidParams(nil)
	}
	if servers == nil {
		servers = []any{}
	}
	for _, value := range servers {
		server, ok := value.(map[string]any)
		if !ok {
			return nil, acp.NewInvalidParams(nil)
		}
		switch server["type"] {
		case "stdio":
			delete(server, "type")
			if server["args"] == nil {
				server["args"] = []any{}
			}
			if server["env"] == nil {
				server["env"] = []any{}
			}
		case "http":
			if !caps.Http {
				return nil, acp.NewInvalidParams(map[string]any{"reason": "HTTP MCP is not advertised"})
			}
			if server["headers"] == nil {
				server["headers"] = []any{}
			}
		default:
			return nil, acp.NewInvalidParams(map[string]any{"reason": "unsupported MCP transport", "type": server["type"]})
		}
	}
	p["mcpServers"] = servers
	return p, nil
}

func v2Config(options []acp.SessionConfigOption, modes *acp.SessionModeState) []any {
	result := make([]any, 0, len(options)+1)
	for _, option := range options {
		entry := object(option)
		entry["configId"] = entry["id"]
		delete(entry, "id")
		if groups, ok := entry["options"].([]any); ok {
			for _, value := range groups {
				if group, ok := value.(map[string]any); ok && group["group"] != nil {
					group["groupId"] = group["group"]
					delete(group, "group")
				}
			}
		}
		result = append(result, entry)
	}
	if modes != nil {
		values := []any{}
		for _, mode := range modes.AvailableModes {
			entry := object(mode)
			entry["value"] = entry["id"]
			delete(entry, "id")
			values = append(values, entry)
		}
		result = append(result, map[string]any{"type": "select", "configId": "mode", "name": "Mode", "category": "mode", "currentValue": modes.CurrentModeId, "options": values})
	}
	return result
}

func v2Commands(value any) any {
	var commands []map[string]any
	decodeObject(value, &commands)
	for _, command := range commands {
		command["type"] = "command"
		if input, ok := command["input"].(map[string]any); ok {
			input["type"] = "text"
		}
	}
	return commands
}

func (c *v2Connection) sendLocked(ctx context.Context, id string, update map[string]any) error {
	if s := c.sessions[id]; s != nil {
		if s.closed && !(update["sessionUpdate"] == "state_update" && update["state"] == "idle" && update["stopReason"] == "cancelled") {
			return nil
		}
		if !s.ready || s.setup != nil {
			s.deferred = append(s.deferred, update)
			return nil
		}
		if update["sessionUpdate"] == "config_option_update" {
			if reflect.DeepEqual(s.lastConfig, update["configOptions"]) {
				return nil
			}
			s.lastConfig = update["configOptions"]
		}
	}
	return c.rpc.SendNotification(ctx, "session/update", map[string]any{"sessionId": id, "update": update})
}

func (c *v2Connection) SessionUpdate(ctx context.Context, p acp.SessionNotification) error {
	if p.Update.AgentMessageChunk != nil || p.Update.AgentThoughtChunk != nil || p.Update.ToolCall != nil {
		c.mu.Lock()
		s := c.sessions[string(p.SessionId)]
		var turn *v2Turn
		if s != nil {
			turn = s.turn
		}
		c.mu.Unlock()
		if turn != nil {
			turn.accept("")
			select {
			case <-turn.acknowledged:
			case <-ctx.Done():
				return ctx.Err()
			case <-c.ctx.Done():
				return c.ctx.Err()
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	id := string(p.SessionId)
	s := c.sessionLocked(id)
	if s.closed {
		return nil
	}
	u := object(p.Update)
	kind, _ := u["sessionUpdate"].(string)
	switch kind {
	case "available_commands_update":
		s.commands = v2Commands(u["availableCommands"])
		u["availableCommands"] = s.commands
		if !s.ready {
			return nil
		}
	case "config_option_update":
		s.config = p.Update.ConfigOptionUpdate.ConfigOptions
		u["configOptions"] = v2Config(s.config, s.modes)
	case "current_mode_update":
		if s.modes == nil {
			return nil
		}
		s.modes.CurrentModeId = p.Update.CurrentModeUpdate.CurrentModeId
		u = map[string]any{"sessionUpdate": "config_option_update", "configOptions": v2Config(s.config, s.modes)}
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
		messageID, _ := u["messageId"].(string)
		if messageID == "" {
			if s.lastKind != kind {
				s.lastMessage = uuid.NewString()
			}
			messageID = s.lastMessage
		}
		s.lastKind = kind
		u["messageId"] = messageID
		key := kind + ":" + messageID
		if _, seen := s.messageIDs[key]; !seen {
			s.messageIDs[key] = messageID
			messageKind := strings.TrimSuffix(kind, "_chunk")
			// Message entities replace content and metadata, avoiding duplicate
			// text when a client resumes with entities it already holds.
			if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": messageKind, "messageId": messageID, "content": []any{}, "_meta": nil}); err != nil {
				return err
			}
		}
	case "tool_call", "tool_call_update":
		u = v2ToolCall(u)
		u["sessionUpdate"] = "tool_call_update"
		if kind == "tool_call" {
			for _, field := range []string{"name", "title", "kind", "status", "content", "locations", "rawInput", "rawOutput", "_meta"} {
				if _, exists := u[field]; !exists {
					u[field] = nil
				}
			}
		}
		s.lastKind = ""
	case "plan":
		u = map[string]any{"sessionUpdate": "plan_update", "plan": map[string]any{"type": "items", "planId": "plan", "entries": u["entries"]}}
	}
	return c.sendLocked(ctx, id, u)
}

func v2ToolCall(call map[string]any) map[string]any {
	for key, value := range call {
		if value == nil {
			delete(call, key)
		}
	}
	delete(call, "sessionUpdate")
	if content, ok := call["content"].([]any); ok {
		for i, value := range content {
			item, ok := value.(map[string]any)
			if !ok || item["type"] != "diff" {
				continue
			}
			path, _ := item["path"].(string)
			oldText, _ := item["oldText"].(string)
			newText, _ := item["newText"].(string)
			operation := "modify"
			if item["oldText"] == nil {
				operation = "add"
			}
			content[i] = map[string]any{"type": "diff", "changes": []any{map[string]any{"operation": operation, "path": path, "fileType": "text"}}, "patch": map[string]any{"format": "git_patch", "text": replacementPatch(path, oldText, newText)}}
		}
	}
	return call
}

func replacementPatch(path, oldText, newText string) string {
	lines := func(text string) []string {
		if text == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	oldLines, newLines := lines(oldText), lines(newText)
	var patch strings.Builder
	fmt.Fprintf(&patch, "diff --git %q %q\n--- %q\n+++ %q\n@@ -%d,%d +%d,%d @@\n", "a/"+path, "b/"+path, "a/"+path, "b/"+path, min(1, len(oldLines)), len(oldLines), min(1, len(newLines)), len(newLines))
	for _, side := range []struct {
		prefix, text string
		lines        []string
	}{{"-", oldText, oldLines}, {"+", newText, newLines}} {
		for _, line := range side.lines {
			patch.WriteString(side.prefix + line + "\n")
		}
		if len(side.lines) > 0 && !strings.HasSuffix(side.text, "\n") {
			patch.WriteString("\\ No newline at end of file\n")
		}
	}
	return patch.String()
}

func (c *v2Connection) interaction(ctx context.Context, id string, async bool) (context.Context, func()) {
	c.mu.Lock()
	s := c.sessionLocked(id)
	combined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	if s.closed {
		cancel()
	} else {
		s.requests.Add(1)
	}
	registered := !s.closed
	stopTurn := func() bool { return false }
	if s.turn != nil {
		stopTurn = context.AfterFunc(s.turn.ctx, cancel)
	}
	if !async && !s.closed {
		s.interactions++
		if s.interactions == 1 {
			_ = c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "state_update", "state": "requires_action"})
		}
	}
	c.mu.Unlock()
	return combined, func() {
		if registered {
			defer s.requests.Done()
		}
		stop()
		stopTurn()
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		if !async && s.interactions > 0 {
			s.interactions--
			if s.interactions == 0 && s.turn != nil && s.turn.ctx.Err() == nil {
				_ = c.sendLocked(c.ctx, id, map[string]any{"sessionUpdate": "state_update", "state": "running"})
			}
		}
	}
}

func (c *v2Connection) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	ctx, finish := c.interaction(ctx, string(p.SessionId), false)
	defer finish()
	call := v2ToolCall(object(p.ToolCall))
	delete(call, "status")
	title, _ := call["title"].(string)
	if title == "" {
		title = "Allow this action?"
	}
	request := map[string]any{"sessionId": p.SessionId, "title": title, "subject": map[string]any{"type": "tool_call", "toolCall": call}, "options": p.Options}
	if p.Meta != nil {
		request["_meta"] = p.Meta
	}
	response, err := acp.SendRequest[map[string]any](c.rpc, ctx, "session/request_permission", request)
	var result acp.RequestPermissionResponse
	if err != nil {
		return result, err
	}
	outcome, _ := response["outcome"].(map[string]any)
	if outcome["outcome"] != "selected" {
		// A dismissed v2 dialog is a refusal, not a session cancellation.
		response["outcome"] = map[string]any{"outcome": "cancelled"}
		if ctx.Err() == nil {
			for _, option := range p.Options {
				if option.Kind == acp.PermissionOptionKindRejectOnce {
					response["outcome"] = map[string]any{"outcome": "selected", "optionId": option.OptionId}
					break
				}
			}
		}
	}
	decodeObject(response, &result)
	return result, nil
}

func (c *v2Connection) UnstableCreateElicitation(ctx context.Context, p acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	request := object(p)
	meta, _ := request["_meta"].(map[string]any)
	id, _ := meta["sessionId"].(string)
	request["sessionId"] = id
	codex, _ := meta["codex"].(map[string]any)
	ctx, finish := c.interaction(ctx, id, codex["delivery"] == "async")
	defer finish()
	return acp.SendRequest[acp.UnstableCreateElicitationResponse](c.rpc, ctx, "elicitation/create", request)
}
