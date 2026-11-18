package acp

import (
	"bytes"
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	_ = decoder.Decode(target)
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&p) != nil || p == nil {
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
		var accept func(string)
		if s != nil {
			turn = s.turn
			if turn != nil {
				accept = turn.accept
			}
		}
		c.mu.Unlock()
		if accept != nil {
			accept("")
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
	request, _ := ctx.Value(v2RequestKey{}).(v2Request)
	if s.replaying && (request.sequence != s.replaySequence || p.Update.Notice != nil || p.Update.UsageUpdate != nil) {
		// Only the load request produces history. Buffer concurrent live
		// updates before assigning message IDs or applying tool transitions.
		// Copy the payload because the caller can reuse it after we return.
		raw, err := json.Marshal(p.Update)
		if err == nil {
			s.afterReplay = append(s.afterReplay, raw)
		}
		return err
	}
	return c.updateLocked(ctx, id, s, p.Update)
}

func (c *v2Connection) updateLocked(ctx context.Context, id string, s *v2Session, update acp.SessionUpdate) error {
	u := object(update)
	kind, _ := u["sessionUpdate"].(string)
	if kind == "tool_call" || kind == "tool_call_update" {
		cwd, _ := s.workspace["cwd"].(string)
		u = v2ToolCall(u, cwd)
	}
	v2UpdateMetadata(u)
	switch kind {
	case "session_info_update":
		for _, key := range []string{"title", "updatedAt"} {
			if value, ok := u[key]; ok {
				s.info[key] = value
			}
		}
		if s.restoring {
			return nil
		}
	case "available_commands_update":
		s.commands, _ = u["availableCommands"].([]any)
		for _, entry := range s.commands {
			command := entry.(map[string]any)
			command["type"] = "command"
			if input, ok := command["input"].(map[string]any); ok {
				input["type"] = "text"
			}
		}
		if !s.ready || s.restoring {
			return nil
		}
	case "config_option_update":
		s.config = update.ConfigOptionUpdate.ConfigOptions
		u["configOptions"] = v2Config(s.config, s.modes)
		if s.restoring {
			return nil
		}
	case "current_mode_update":
		if s.modes == nil {
			return nil
		}
		s.modes.CurrentModeId = update.CurrentModeUpdate.CurrentModeId
		if s.restoring {
			return nil
		}
		u = map[string]any{"sessionUpdate": "config_option_update", "configOptions": v2Config(s.config, s.modes)}
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
		messageID, _ := u["messageId"].(string)
		if messageID == "" {
			if s.lastKind != kind {
				if s.replaying {
					s.replayMessage++
					s.lastMessage = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%s/%d", id, kind, s.replayMessage))).String()
				} else {
					s.lastMessage = uuid.NewString()
				}
			}
			messageID = s.lastMessage
		}
		s.lastKind, s.lastMessage = kind, messageID
		u["messageId"] = messageID
		key := kind + ":" + messageID
		if !s.reportedEntities[key] {
			s.reportedEntities[key] = true
			messageKind := strings.TrimSuffix(kind, "_chunk")
			// Message entities replace content and metadata, avoiding duplicate
			// text when a client resumes with entities it already holds.
			if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": messageKind, "messageId": messageID, "content": []any{}, "_meta": nil}); err != nil {
				return err
			}
		}
	case "tool_call", "tool_call_update":
		u["sessionUpdate"] = "tool_call_update"
		toolID, _ := u["toolCallId"].(string)
		previous, seen := s.toolStatuses[toolID]
		key := "tool:" + toolID
		firstReplay := s.replaying && !s.reportedEntities[key]
		if firstReplay {
			s.reportedEntities[key] = true
			if u["_meta"] == nil {
				u["_meta"] = nil
			} else if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": toolID, "_meta": nil}); err != nil {
				return err
			}
		}
		if kind == "tool_call" && (!seen || firstReplay) {
			for _, field := range []string{"name", "title", "kind", "status", "content", "locations", "rawInput", "rawOutput", "_meta"} {
				if _, exists := u[field]; !exists {
					u[field] = nil
				}
			}
		}
		if status, present := u["status"]; present {
			next, _ := status.(string)
			if next == "" {
				next = "pending"
			}
			switch {
			case previous == "completed" || previous == "failed" || previous == "cancelled",
				seen && previous != "pending" && next == "pending":
				// Replay may show earlier states. Live patches may add output
				// to a finished call, but cannot restart it or reset its status.
				if !s.replaying && next != previous {
					delete(u, "status")
				}
			default:
				s.toolStatuses[toolID] = next
			}
		} else if !seen {
			s.toolStatuses[toolID] = "pending"
		}
		s.lastKind = ""
	case "plan":
		u = map[string]any{"sessionUpdate": "plan_update", "plan": map[string]any{"type": "items", "planId": "plan", "entries": u["entries"]}}
	}
	return c.sendLocked(ctx, id, u)
}

// Backend annotations on v1 updates predate the v2 metadata namespace.
// Leave application payloads and the values of registered metadata keys opaque.
func v2UpdateMetadata(value any) {
	switch node := value.(type) {
	case []any:
		for _, v := range node {
			v2UpdateMetadata(v)
		}
	case map[string]any:
		for k, v := range node {
			switch k {
			case "_meta":
				if fields, ok := v.(map[string]any); ok {
					for name := range fields {
						prefix, local, ok := strings.Cut(name, "/")
						if !ok || local == "" || !strings.Contains(prefix, ".") {
							delete(fields, name)
						}
					}
					if len(fields) == 0 {
						delete(node, k)
					}
				}
			case "rawInput", "rawOutput", "data", "structuredContent":
			default:
				v2UpdateMetadata(v)
			}
		}
	}
}

func v2ToolCall(call map[string]any, cwd string) map[string]any {
	delete(call, "sessionUpdate")
	if locations, ok := call["locations"].([]any); ok {
		for _, location := range locations {
			entry, _ := location.(map[string]any)
			if path, ok := entry["path"].(string); ok {
				entry["path"] = ResolvePath(path, cwd)
			}
		}
	}
	if content, ok := call["content"].([]any); ok {
		for i, value := range content {
			item, ok := value.(map[string]any)
			if !ok || item["type"] != "diff" {
				continue
			}
			path, _ := item["path"].(string)
			path = ResolvePath(path, cwd)
			oldText, _ := item["oldText"].(string)
			newText, _ := item["newText"].(string)
			operation := "modify"
			if item["oldText"] == nil {
				operation = "add"
			}
			meta, _ := item["_meta"].(map[string]any)
			if meta["kind"] == "delete" {
				operation = "delete"
			}
			diff := map[string]any{"type": "diff", "changes": []any{map[string]any{"operation": operation, "path": path, "fileType": "text"}}}
			if meta != nil {
				diff["_meta"] = meta
			}
			if oldText != newText {
				diff["patch"] = map[string]any{"format": "git_patch", "text": replacementPatch(path, operation, oldText, newText)}
			}
			content[i] = diff
		}
	}
	return call
}

func replacementPatch(path, operation, oldText, newText string) string {
	lines := func(text string) []string {
		if text == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	oldLines, newLines := lines(oldText), lines(newText)
	var patch strings.Builder
	quoted := gitPatchPath(path)
	oldPath, newPath := quoted, quoted
	if operation == "add" {
		oldPath = "/dev/null"
	} else if operation == "delete" {
		newPath = "/dev/null"
	}
	fmt.Fprintf(&patch, "diff --git %s %s\n--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", quoted, quoted, oldPath, newPath, min(1, len(oldLines)), len(oldLines), min(1, len(newLines)), len(newLines))
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

// Git quotes unusual bytes with three octal digits, including each UTF-8
// byte. Go string quoting would emit escapes Git cannot read.
func gitPatchPath(path string) string {
	if !strings.ContainsFunc(path, func(r rune) bool { return r <= ' ' || r >= 0x7f || r == '"' || r == '\\' }) {
		return path
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	for i := range len(path) {
		b := path[i]
		switch {
		case b == '"' || b == '\\':
			quoted.WriteByte('\\')
			quoted.WriteByte(b)
		case b < ' ' || b >= 0x7f:
			fmt.Fprintf(&quoted, "\\%03o", b)
		default:
			quoted.WriteByte(b)
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
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
	foreground := !async && !s.closed && s.turn != nil
	if foreground {
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
		if foreground {
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
	c.mu.Lock()
	cwd, _ := c.sessionLocked(string(p.SessionId)).workspace["cwd"].(string)
	c.mu.Unlock()
	call := v2ToolCall(object(p.ToolCall), cwd)
	delete(call, "status")
	title, _ := call["title"].(string)
	if title == "" {
		title = "Allow this action?"
	}
	request := map[string]any{"sessionId": p.SessionId, "title": title, "subject": map[string]any{"type": "tool_call", "toolCall": call}, "options": p.Options}
	if p.Meta != nil {
		request["_meta"] = p.Meta
	}
	wireRequest := object(request)
	v2UpdateMetadata(wireRequest)
	response, err := acp.SendRequest[map[string]any](c.rpc, ctx, "session/request_permission", wireRequest)
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

func (c *v2Connection) CreateElicitation(ctx context.Context, p acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	var session *acp.ElicitationSessionScope
	if p.Form != nil {
		session = p.Form.Session
	} else if p.Url != nil {
		session = p.Url.Session
	}
	if session != nil {
		background, _ := ctx.Value(backgroundInteractionKey{}).(bool)
		var finish func()
		ctx, finish = c.interaction(ctx, string(session.SessionId), background)
		defer finish()
	}
	wireRequest := object(p)
	if p.Form != nil {
		// ACP v2 removed the string pattern keyword. Keep the original MCP
		// schema intact for the server that validates the answer.
		schema, _ := wireRequest["requestedSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		for _, value := range properties {
			property, _ := value.(map[string]any)
			if property["type"] == "string" {
				delete(property, "pattern")
			}
		}
	}
	v2UpdateMetadata(wireRequest)
	return acp.SendRequest[acp.CreateElicitationResponse](c.rpc, ctx, "elicitation/create", wireRequest)
}
