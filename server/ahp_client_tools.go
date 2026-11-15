package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpClientCall is a call to a tool that an active client provides and runs.
type ahpClientCall struct {
	client  string
	started bool                       // the agent asked for it; until then the client must not run it
	input   string                     // the model's arguments, which the transcript shortens for display
	meta    map[string]json.RawMessage // the client's own, once it sent any
	content []t.ToolResultContent      // what the client showed while it ran
	result  *t.ToolCallResult
	done    chan struct{} // closed with the result
}

// Model APIs take no other tool names.
var ahpToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// offerTools gives the agent the tools of the session's active clients for its
// next turn. The first client to offer a name keeps it.
func (a *ahpChat) offerTools() []tool.Tool {
	a.project.mu.Lock()
	defer a.project.mu.Unlock()
	a.offered = map[string]string{}
	var tools []tool.Tool
	for _, client := range a.session.state.ActiveClients {
		for _, def := range client.Tools {
			if !ahpToolName.MatchString(def.Name) || a.offered[def.Name] != "" {
				continue
			}
			a.offered[def.Name] = client.ClientId
			tools = append(tools, a.clientTool(def))
		}
	}
	return tools
}

func (a *ahpChat) clientTool(def t.ToolDefinition) tool.Tool {
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if def.InputSchema != nil {
		var schema map[string]any
		if json.Unmarshal(*def.InputSchema, &schema) == nil && schema != nil {
			parameters = schema
		}
	}
	// The client asks its own user before it runs a tool; plan mode trusts its hint.
	effect := tool.EffectMutates
	if hints := def.Annotations; hints != nil && hints.ReadOnlyHint != nil && *hints.ReadOnlyHint {
		effect = tool.EffectReadOnly
	}
	var description string
	if def.Description != nil {
		description = *def.Description
	}
	return tool.Tool{Name: def.Name, Description: description, Parameters: parameters, Effect: tool.StaticEffect(effect),
		Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
			return a.runClientTool(ctx, def.Name)
		},
	}
}

// runClientTool waits until the client that provides a tool completes the call.
func (a *ahpChat) runClientTool(ctx context.Context, name string) (tool.Result, error) {
	h := a.project
	request, _ := hook.ToolCallFromContext(ctx)
	h.mu.Lock()
	call := a.clientCalls[request.ID]
	if call == nil {
		call = &ahpClientCall{client: a.offered[name], done: make(chan struct{})}
		a.clientCalls[request.ID] = call
	}
	call.started, call.input = true, request.Args
	h.mu.Unlock()
	c := a.controller
	c.mu.Lock()
	a.syncLocked(c, nil) // the call shows as running
	c.mu.Unlock()
	h.mu.Lock()
	if !slices.ContainsFunc(a.session.state.ActiveClients, func(c t.SessionActiveClient) bool { return c.ClientId == call.client }) {
		a.failClientCall(request.ID, call, "The client that provides "+name+" has disconnected.")
	}
	h.mu.Unlock()
	select {
	case <-call.done:
	case <-ctx.Done():
		h.mu.Lock()
		a.failClientCall(request.ID, call, "The call was stopped before the client completed it.")
		h.mu.Unlock()
		return tool.Result{}, ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return clientToolResult(*call.result), nil
}

// clientToolResult is what the model reads of a client's result.
func clientToolResult(result t.ToolCallResult) tool.Result {
	var content tool.Content
	for _, block := range result.Content {
		switch c := block.Value.(type) {
		case *t.ToolResultTextContent:
			content = append(content, tool.TextBlock(c.Text))
		case *t.ToolResultEmbeddedResourceContent:
			image := tool.ImageContent{MIMEType: c.ContentType}
			if image.Data, _ = base64.StdEncoding.DecodeString(c.Data); image.Valid() {
				content = append(content, tool.ContentBlock{Image: &image})
			}
		case *t.ToolResultResourceContent:
			content = append(content, tool.TextBlock(c.Uri))
		}
	}
	if strings.TrimSpace(content.Text()) == "" && len(result.StructuredContent) > 0 {
		data, _ := json.Marshal(result.StructuredContent)
		content = append(content, tool.TextBlock(string(data)))
	}
	if result.Error != nil {
		content = append(content, tool.TextBlock("error: "+string(*result.Error)))
	}
	if len(content) == 0 {
		content = tool.TextContent(result.PastTenseMessage.AsText())
	}
	return tool.Result{Content: content, IsError: !result.Success}
}

// runningCall finds a running tool call of the active turn. Caller holds host.mu.
func (a *ahpChat) runningCall(turn, id string) *t.ToolCallRunningState {
	active := a.chat.ActiveTurn
	if active == nil || active.Id != turn {
		return nil
	}
	for _, part := range active.ResponseParts {
		if p, ok := part.Value.(*t.ToolCallResponsePart); ok {
			if call, ok := p.ToolCall.Value.(*t.ToolCallRunningState); ok && call.ToolCallId == id {
				return call
			}
		}
	}
	return nil
}

// ownedCall finds a call that waits for a client. Caller holds host.mu.
func (a *ahpChat) ownedCall(chat bool, client, turn, id string) (*ahpClientCall, error) {
	call := a.clientCalls[id]
	if !chat || call == nil || call.result != nil || a.runningCall(turn, id) == nil {
		return nil, ahpInvalid("tool call is not waiting for a client in this chat")
	}
	if call.client != client {
		return nil, ahpInvalid("only the client that provides the tool reports on its calls")
	}
	return call, nil
}

func (c *ahpClientCall) complete(result t.ToolCallResult, meta map[string]json.RawMessage) {
	c.result = &result
	if meta != nil {
		c.meta = meta
	}
	close(c.done)
}

// failClientCall completes a call for a client that no longer can. Caller holds host.mu.
func (a *ahpChat) failClientCall(id string, call *ahpClientCall, message string) {
	if call.result != nil {
		return
	}
	data, _ := json.Marshal(map[string]string{"message": message})
	failure := json.RawMessage(data)
	call.complete(t.ToolCallResult{Success: false, PastTenseMessage: t.NewStringOrMarkdownPlain(message), Error: &failure}, nil)
	if active := a.chat.ActiveTurn; active != nil && a.runningCall(active.Id, id) != nil {
		previous := a.session.summary()
		a.chatAction(t.StateAction{Value: &t.ChatToolCallCompleteAction{Type: t.ActionTypeChatToolCallComplete, TurnId: active.Id, ToolCallId: id, Result: *call.result}})
		a.session.syncChats(previous)
	}
}

// clientLeft fails the calls that wait for a client. Caller holds host.mu.
func (a *ahpChat) clientLeft(client string) {
	if a.chat.ActiveTurn == nil {
		return
	}
	for _, part := range slices.Clone(a.chat.ActiveTurn.ResponseParts) {
		if p, ok := part.Value.(*t.ToolCallResponsePart); ok {
			running, _ := p.ToolCall.Value.(*t.ToolCallRunningState)
			if running == nil {
				continue
			}
			if call := a.clientCalls[running.ToolCallId]; call != nil && call.client == client {
				a.failClientCall(running.ToolCallId, call, "The client that provides "+running.ToolName+" has disconnected.")
			}
		}
	}
}

// entryPart projects an entry. A call to a client's tool shows what the client
// reported, and is the client's from the moment the model starts it.
func (a *ahpChat) entryPart(e TranscriptEntry) (t.ResponsePart, bool) {
	if e.Type != "tool" {
		return a.project.entryPart(e)
	}
	id := cmp.Or(e.ToolID, e.ID)
	call := a.clientCalls[id]
	if call == nil && e.ToolResult == nil && a.offered[e.ToolName] != "" {
		call = &ahpClientCall{client: a.offered[e.ToolName], done: make(chan struct{})}
		a.clientCalls[id] = call
	}
	if call == nil {
		return a.project.entryPart(e)
	}
	if e.ToolResult == nil {
		if call.result != nil {
			e.ToolResult = new(string) // the client is done before the agent says so
		} else if !call.started {
			e.ToolPartial = true
		}
	}
	part, ok := a.project.entryPart(e)
	contributor := &t.ToolCallContributor{Value: &t.ToolCallClientContributor{Kind: t.ToolCallContributorKindClient, ClientId: call.client}}
	switch v := part.Value.(*t.ToolCallResponsePart).ToolCall.Value.(type) {
	case *t.ToolCallStreamingState:
		v.Contributor = contributor
	case *t.ToolCallRunningState:
		v.Contributor, v.Content, v.ToolInput = contributor, call.content, &t.ToolInput{Inline: &call.input}
		if call.meta != nil {
			v.Meta = call.meta
		}
	case *t.ToolCallCompletedState:
		v.Contributor = contributor
		if call.started {
			v.ToolInput = &t.ToolInput{Inline: &call.input}
		}
		if call.meta != nil {
			v.Meta = call.meta
		}
		if r := call.result; r != nil {
			v.Success, v.PastTenseMessage, v.StructuredContent, v.ResultMetadata, v.Error = r.Success, r.PastTenseMessage, r.StructuredContent, r.ResultMetadata, r.Error
			v.Content = append([]t.ToolResultContent(nil), r.Content...) // as the reducer keeps it
		}
	}
	return part, ok
}
