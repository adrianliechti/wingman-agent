package server

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type ahpSession struct {
	host             *ahpHost
	controller       *sessionController
	resource         string
	created          string
	state            t.SessionState
	chat             t.ChatState
	deleted          bool
	prompts          map[string]*ahpPrompt
	pending          map[string]*ahpPendingAction
	projectedInput   string
	projectedOutcome string
	projectedPrompts []PromptView
}

type ahpPrompt struct {
	view     PromptView
	turn     string
	response *Command
	answers  map[string]t.ChatInputAnswer
}

type ahpPendingAction struct {
	params   t.DispatchActionParams
	origin   t.ActionOrigin
	envelope *t.ActionEnvelope
	receipt  *ahpActionReceipt
}

func newAHPSession(h *ahpHost, c *sessionController, resource, title string, updated time.Time) *ahpSession {
	stamp := updated.UTC().Format(time.RFC3339Nano)
	chatURI := ahpChatURI(resource)
	return &ahpSession{host: h, controller: c, resource: resource, created: stamp, prompts: map[string]*ahpPrompt{}, pending: map[string]*ahpPendingAction{},
		state: t.SessionState{Changesets: h.changesetCatalogue(), Provider: c.ref.BackendID, Title: title, Status: t.SessionStatusIdle, Lifecycle: t.SessionLifecycleReady, ActiveClients: []t.SessionActiveClient{}, Chats: []t.ChatSummary{{Resource: chatURI, Title: title, Status: t.SessionStatusIdle, ModifiedAt: stamp}}, DefaultChat: &chatURI, WorkingDirectories: []string{h.directoryURI()}, Config: ahpConfig(c.state.Settings)},
		chat:  t.ChatState{Resource: chatURI, Title: title, Status: t.SessionStatusIdle, ModifiedAt: stamp, Turns: []t.Turn{}, Config: &t.ChatConfigState{Values: map[string]json.RawMessage{}}},
	}
}

func ahpSummaryMeta(ref SessionRef) map[string]json.RawMessage {
	data, _ := json.Marshal(ref)
	return map[string]json.RawMessage{"wingman.ref": data}
}

func (a *ahpSession) summary() t.SessionSummary {
	return t.SessionSummary{Meta: ahpSummaryMeta(a.controller.ref), Resource: a.resource, Provider: a.state.Provider, Title: a.state.Title, Status: a.chat.Status, CreatedAt: a.created, ModifiedAt: a.chat.ModifiedAt, WorkingDirectories: a.state.WorkingDirectories, DefaultChat: a.state.DefaultChat, Chats: []t.SessionChatSummary{{Resource: a.chat.Resource, Title: a.chat.Title, Status: &a.chat.Status}}}
}

func (a *ahpSession) emit(channel string, action t.StateAction) {
	var origin *t.ActionOrigin
	p := a.pending[ahpActionKey(action)]
	if p != nil && p.envelope == nil && channel == p.params.Channel {
		origin = &p.origin
	}
	envelope := a.host.publish(channel, action, origin, nil)
	if origin != nil {
		p.envelope = &envelope
		p.receipt.envelope = &envelope
		delete(a.pending, ahpActionKey(action))
	}
}

func ahpActionKey(action t.StateAction) string {
	switch v := action.Value.(type) {
	case *t.ChatPendingMessageSetAction:
		return "pending-set:" + v.Id
	case *t.ChatPendingMessageRemovedAction:
		return "pending-remove:" + v.Id
	case *t.ChatTurnStartedAction:
		return "start:" + v.TurnId
	case *t.ChatTurnCancelledAction:
		return "cancel:" + v.TurnId
	case *t.ChatToolCallConfirmedAction:
		return "confirm:" + v.ToolCallId
	case *t.ChatInputCompletedAction:
		return "input:" + v.RequestId
	default:
		data, _ := json.Marshal(action)
		return string(data)
	}
}

func (a *ahpSession) chatAction(action t.StateAction) {
	ahp.ApplyActionToChat(&a.chat, action)
	a.emit(a.chat.Resource, action)
}

func (a *ahpSession) sessionAction(action t.StateAction) {
	ahp.ApplyActionToSession(&a.state, action)
	a.emit(a.resource, action)
}

func (a *ahpSession) removeClient(id string) {
	for _, client := range a.state.ActiveClients {
		if client.ClientId == id {
			a.sessionAction(t.StateAction{Value: &t.SessionActiveClientRemovedAction{Type: t.ActionTypeSessionActiveClientRemoved, ClientId: id}})
			return
		}
	}
}

func (a *ahpSession) syncLocked(c *sessionController, changes []SessionChange) {
	a.host.mu.Lock()
	defer a.host.mu.Unlock()
	if a.deleted {
		return
	}
	if c.state.Status == "deleted" {
		a.deleted = true
		a.host.notifyRoot("root/sessionRemoved", t.SessionRemovedParams{Channel: t.RootResourceURI, Session: a.resource})
		return
	}
	// Token deltas do not rebuild or copy the completed conversation.
	if len(changes) == 1 && changes[0].Type == "entry.append" && a.chat.ActiveTurn != nil {
		change := changes[0]
		for _, part := range a.chat.ActiveTurn.ResponseParts {
			switch p := part.Value.(type) {
			case *t.MarkdownResponsePart:
				if p.Id == change.ID {
					a.chatAction(t.StateAction{Value: &t.ChatDeltaAction{Type: t.ActionTypeChatDelta, TurnId: a.chat.ActiveTurn.Id, PartId: p.Id, Content: change.Text}})
					return
				}
			case *t.ReasoningResponsePart:
				if p.Id == change.ID {
					a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: a.chat.ActiveTurn.Id, PartId: p.Id, Content: change.Text}})
					return
				}
			}
		}
	}
	a.syncProjection(c, changes)
}

func (a *ahpSession) syncProjection(c *sessionController, changes []SessionChange) {
	previousSummary := a.summary()
	transcriptChanged := changes == nil || a.projectedInput != c.activeInput || a.projectedOutcome != c.lastOutcome || !reflect.DeepEqual(a.projectedPrompts, c.state.Prompts)
	for _, change := range changes {
		if change.Type != "state.replace" {
			transcriptChanged = true
		}
	}
	if transcriptChanged {
		a.projectedInput, a.projectedOutcome = c.activeInput, c.lastOutcome
		a.projectedPrompts = slices.Clone(c.state.Prompts)
		for _, p := range c.state.Prompts {
			if a.prompts[p.ID] == nil {
				turn := c.activeInput
				if a.chat.ActiveTurn != nil {
					turn = a.chat.ActiveTurn.Id
				}
				a.prompts[p.ID] = &ahpPrompt{view: p, turn: turn}
			}
		}
		for id, p := range a.prompts {
			if p.response == nil && !slices.ContainsFunc(c.state.Prompts, func(v PromptView) bool { return v.ID == id }) {
				p.response = &Command{Action: "cancel"}
			}
		}
		turns, active := a.transcript(c)
		if old := a.chat.ActiveTurn; old != nil && (active == nil || active.Id != old.Id) {
			state := t.TurnStateComplete
			if len(turns) > 0 {
				state = turns[len(turns)-1].State
			}
			a.finishTurn(old.Id, state, c.state.Error)
		}
		if !reflect.DeepEqual(a.chat.Turns, turns) {
			a.chatAction(t.StateAction{Value: &t.ChatTruncatedAction{Type: t.ActionTypeChatTruncated}})
			if active == nil && len(turns) > 0 && turns[len(turns)-1].State == t.TurnStateError {
				last := turns[len(turns)-1]
				a.chatAction(t.StateAction{Value: &t.ChatTurnsLoadedAction{Type: t.ActionTypeChatTurnsLoaded, Turns: turns[:len(turns)-1]}})
				a.chatAction(t.StateAction{Value: &t.ChatTurnStartedAction{Type: t.ActionTypeChatTurnStarted, TurnId: last.Id, StartedAt: *last.StartedAt, Message: last.Message}})
				for _, part := range last.ResponseParts {
					if _, isError := part.Value.(*t.ErrorResponsePart); !isError {
						a.chatAction(t.StateAction{Value: &t.ChatResponsePartAction{Type: t.ActionTypeChatResponsePart, TurnId: last.Id, Part: part}})
					}
				}
				a.finishTurn(last.Id, t.TurnStateError, c.state.Error)
			} else {
				a.chatAction(t.StateAction{Value: &t.ChatTurnsLoadedAction{Type: t.ActionTypeChatTurnsLoaded, Turns: turns}})
			}
		}
		if active != nil {
			if a.chat.ActiveTurn == nil || a.chat.ActiveTurn.Id != active.Id {
				a.chatAction(t.StateAction{Value: &t.ChatTurnStartedAction{Type: t.ActionTypeChatTurnStarted, TurnId: active.Id, StartedAt: active.StartedAt, Message: active.Message}})
			}
			if !a.syncParts(active.ResponseParts) {
				var keep *string
				if len(a.chat.Turns) > 0 {
					keep = &a.chat.Turns[len(a.chat.Turns)-1].Id
				}
				a.chatAction(t.StateAction{Value: &t.ChatTruncatedAction{Type: t.ActionTypeChatTruncated, TurnId: keep}})
				a.chatAction(t.StateAction{Value: &t.ChatTurnStartedAction{Type: t.ActionTypeChatTurnStarted, TurnId: active.Id, StartedAt: active.StartedAt, Message: active.Message}})
				a.syncParts(active.ResponseParts)
			}
		}
	}
	config := ahpConfig(c.state.Settings)
	if a.state.Config == nil {
		a.state.Config = config
	}
	if !reflect.DeepEqual(a.state.Config.Values, config.Values) {
		a.sessionAction(t.StateAction{Value: &t.SessionConfigChangedAction{Type: t.ActionTypeSessionConfigChanged, Config: config.Values, Replace: ahpPtr(true)}})
	}
	// Configuration choices can change when a provider discovers models or a
	// model changes its effort options. Snapshots must always carry the new schema.
	a.state.Config.Schema = config.Schema
	a.syncRuntime(c)
	a.syncQueue(c)
	a.syncInputNeeded()
	if a.state.Title == "New session" && len(c.entries) > 0 {
		for _, e := range c.entries {
			if e.Type == "user" && e.Content != "" {
				title := []rune(strings.SplitN(e.Content, "\n", 2)[0])
				if len(title) > 80 {
					title = title[:80]
				}
				a.sessionAction(t.StateAction{Value: &t.SessionTitleChangedAction{Type: t.ActionTypeSessionTitleChanged, Title: string(title)}})
				break
			}
		}
	}
	a.state.Status = a.chat.Status
	chatSummary := t.ChatSummary{Resource: a.chat.Resource, Title: a.chat.Title, Status: a.chat.Status, ModifiedAt: a.chat.ModifiedAt}
	if len(a.state.Chats) == 0 || !reflect.DeepEqual(a.state.Chats[0], chatSummary) {
		a.sessionAction(t.StateAction{Value: &t.SessionChatUpdatedAction{Type: t.ActionTypeSessionChatUpdated, Chat: a.chat.Resource, Changes: t.PartialChatSummary{Title: &chatSummary.Title, Status: &chatSummary.Status, ModifiedAt: &chatSummary.ModifiedAt}}})
	}
	if next := a.summary(); !reflect.DeepEqual(previousSummary, next) {
		a.host.notifyRoot("root/sessionSummaryChanged", t.SessionSummaryChangedParams{Channel: t.RootResourceURI, Session: a.resource, Changes: t.PartialSessionSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt, Chats: next.Chats}})
	}
}

func (a *ahpSession) finishTurn(id string, state t.TurnState, message *string) {
	switch state {
	case t.TurnStateCancelled:
		a.chatAction(t.StateAction{Value: &t.ChatTurnCancelledAction{Type: t.ActionTypeChatTurnCancelled, TurnId: id}})
	case t.TurnStateError:
		text := "Turn failed"
		if message != nil {
			text = *message
		}
		a.chatAction(t.StateAction{Value: &t.ChatErrorAction{Type: t.ActionTypeChatError, TurnId: id, Part: t.ErrorResponsePart{Kind: t.ResponsePartKindError, Error: t.ErrorInfo{ErrorType: "agent", Message: text}}}})
	default:
		a.chatAction(t.StateAction{Value: &t.ChatTurnCompleteAction{Type: t.ActionTypeChatTurnComplete, TurnId: id}})
	}
}

func (a *ahpSession) transcript(c *sessionController) ([]t.Turn, *t.ActiveTurn) {
	old := append([]t.Turn{}, a.chat.Turns...)
	if active := a.chat.ActiveTurn; active != nil {
		old = append(old, t.Turn{Id: active.Id, StartedAt: &active.StartedAt, Message: active.Message, ResponseParts: active.ResponseParts, State: t.TurnStateComplete})
	}
	turns := []t.Turn{}
	for _, e := range c.entries {
		if e.Type == "user" {
			if c.activeInput != "" && len(turns) > 0 && turns[len(turns)-1].Id == c.activeInput && e.InputID != c.activeInput {
				last := &turns[len(turns)-1]
				input, _ := json.Marshal(e)
				last.ResponseParts = append(last.ResponseParts, t.ResponsePart{Value: &t.SystemNotificationResponsePart{Kind: t.ResponsePartKindSystemNotification, Content: t.NewStringOrMarkdownPlain("User follow-up: " + e.Content), Meta: map[string]json.RawMessage{"wingman.input": input}}})
				continue
			}
			id := e.InputID
			if id == "" {
				id = e.ID
			}
			stamp := a.created
			message := a.host.message(e.Content, e.Files, e.Images)
			identity, _ := json.Marshal(map[string]string{"id": e.ID, "messageId": e.MessageID})
			message.Meta = map[string]json.RawMessage{"wingman.entry": identity}
			if pending := a.pending["start:"+id]; pending != nil {
				if started, ok := pending.params.Action.Value.(*t.ChatTurnStartedAction); ok {
					message = started.Message
				}
			}
			if len(turns) < len(old) {
				prior := old[len(turns)]
				// External backends may omit InputID in their committed history.
				// Preserve turn identity at the controller's ordered history boundary.
				if e.InputID == "" {
					id = prior.Id
				}
				if prior.Id == id {
					if prior.StartedAt != nil {
						stamp = *prior.StartedAt
					}
					message = prior.Message
					message.Text = e.Content
				}
			} else {
				stamp = time.Now().UTC().Format(time.RFC3339Nano)
			}
			turns = append(turns, t.Turn{Id: id, StartedAt: &stamp, Duration: ahpPtr(int64(0)), Message: message, ResponseParts: []t.ResponsePart{}, State: t.TurnStateComplete})
			continue
		}
		if len(turns) == 0 {
			continue
		}
		if part, ok := ahpEntryPart(e); ok {
			last := &turns[len(turns)-1]
			last.ResponseParts = append(last.ResponseParts, part)
		}
	}
	for i := range turns {
		turn := &turns[i]
		if i < len(old) && old[i].Id == turn.Id {
			turn.State = old[i].State
		}
		ids := []string{}
		for id, p := range a.prompts {
			if p.turn == turn.Id {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			turn.ResponseParts = append(turn.ResponseParts, ahpPromptPart(a.prompts[id]))
		}
	}
	if len(turns) == 0 {
		return turns, nil
	}
	last := &turns[len(turns)-1]
	if c.activeInput != "" {
		last.Id = c.activeInput
		return turns[:len(turns)-1], &t.ActiveTurn{Id: last.Id, StartedAt: *last.StartedAt, Message: last.Message, ResponseParts: last.ResponseParts}
	}
	switch code.TurnInputState(c.lastOutcome) {
	case code.TurnInputCancelled:
		last.State = t.TurnStateCancelled
	case code.TurnInputFailed:
		last.State = t.TurnStateError
	}
	for i := range turns {
		turn := &turns[i]
		if turn.State == t.TurnStateError && i == len(turns)-1 {
			message := "Turn failed"
			if c.state.Error != nil {
				message = *c.state.Error
			}
			turn.ResponseParts = append(turn.ResponseParts, t.ResponsePart{Value: &t.ErrorResponsePart{Kind: t.ResponsePartKindError, Error: t.ErrorInfo{ErrorType: "agent", Message: message}}})
		}
	}
	return turns, nil
}

func ahpEntryPart(e TranscriptEntry) (t.ResponsePart, bool) {
	switch e.Type {
	case "assistant":
		content := e.Content
		for _, image := range e.Images {
			content += "\n\n![Image](" + image + ")"
		}
		return t.ResponsePart{Value: &t.MarkdownResponsePart{Kind: t.ResponsePartKindMarkdown, Id: e.ID, Content: content}}, true
	case "reasoning":
		return t.ResponsePart{Value: &t.ReasoningResponsePart{Kind: t.ResponsePartKindReasoning, Id: e.ID, Content: e.Content}}, true
	case "tool":
		id := e.ToolID
		if id == "" {
			id = e.ID
		}
		metadata, _ := json.Marshal(map[string]any{"id": e.ID, "messageId": e.MessageID, "images": e.Images, "toolKind": e.ToolKind, "toolLocations": e.ToolLocations, "toolHint": e.ToolHint})
		meta := map[string]json.RawMessage{"wingman": metadata}
		// VS Code's optional presentation conventions. These are rendering hints;
		// ownership, permissions, and tool results remain standard AHP state.
		switch e.ToolKind {
		case "execute":
			meta["toolKind"] = json.RawMessage(`"terminal"`)
		case "read", "search":
			meta["toolKind"], _ = json.Marshal(e.ToolKind)
		}
		invocation := t.NewStringOrMarkdownPlain(e.ToolName)
		input := &t.ToolInput{Inline: &e.ToolArgs}
		var call t.ToolCallState
		if e.ToolResult != nil {
			call.Value = &t.ToolCallCompletedState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Status: t.ToolCallStatusCompleted, InvocationMessage: invocation, ToolInput: input, Confirmed: t.ToolCallConfirmationReasonNotNeeded, Success: true, PastTenseMessage: invocation, Content: []t.ToolResultContent{{Value: &t.ToolResultTextContent{Type: t.ToolResultContentTypeText, Text: *e.ToolResult}}}}
		} else if e.ToolPartial {
			call.Value = &t.ToolCallStreamingState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Status: t.ToolCallStatusStreaming, PartialInput: &e.ToolArgs}
		} else {
			call.Value = &t.ToolCallRunningState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Status: t.ToolCallStatusRunning, InvocationMessage: invocation, ToolInput: input, Confirmed: t.ToolCallConfirmationReasonNotNeeded}
		}
		return t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}, true
	}
	return t.ResponsePart{}, false
}

func (a *ahpSession) syncParts(parts []t.ResponsePart) bool {
	active := a.chat.ActiveTurn
	if len(active.ResponseParts) > len(parts) {
		return false
	}
	for i, next := range parts {
		if i >= len(active.ResponseParts) {
			if call, ok := next.Value.(*t.ToolCallResponsePart); ok {
				a.appendTool(active.Id, call.ToolCall)
			} else if input, ok := next.Value.(*t.InputRequestResponsePart); ok && input.Response == nil {
				a.chatAction(t.StateAction{Value: &t.ChatInputRequestedAction{Type: t.ActionTypeChatInputRequested, Request: input.Request}})
			} else {
				a.chatAction(t.StateAction{Value: &t.ChatResponsePartAction{Type: t.ActionTypeChatResponsePart, TurnId: active.Id, Part: next}})
			}
			continue
		}
		previous := active.ResponseParts[i]
		if reflect.DeepEqual(previous, next) {
			continue
		}
		switch n := next.Value.(type) {
		case *t.MarkdownResponsePart:
			p, ok := previous.Value.(*t.MarkdownResponsePart)
			if !ok || p.Id != n.Id || !strings.HasPrefix(n.Content, p.Content) {
				return false
			}
			a.chatAction(t.StateAction{Value: &t.ChatDeltaAction{Type: t.ActionTypeChatDelta, TurnId: active.Id, PartId: n.Id, Content: strings.TrimPrefix(n.Content, p.Content)}})
		case *t.ReasoningResponsePart:
			p, ok := previous.Value.(*t.ReasoningResponsePart)
			if !ok || p.Id != n.Id || !strings.HasPrefix(n.Content, p.Content) {
				return false
			}
			a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: active.Id, PartId: n.Id, Content: strings.TrimPrefix(n.Content, p.Content)}})
		case *t.ToolCallResponsePart:
			p, ok := previous.Value.(*t.ToolCallResponsePart)
			if !ok {
				return false
			}
			if !a.syncTool(active.Id, p.ToolCall, n.ToolCall) {
				return false
			}
		case *t.InputRequestResponsePart:
			p, ok := previous.Value.(*t.InputRequestResponsePart)
			if !ok || p.Request.Id != n.Request.Id || n.Response == nil {
				return false
			}
			a.chatAction(t.StateAction{Value: &t.ChatInputCompletedAction{Type: t.ActionTypeChatInputCompleted, RequestId: n.Request.Id, Response: *n.Response, Answers: n.Request.Answers}})
		default:
			return false
		}
	}
	return true
}

func (a *ahpSession) appendTool(turn string, call t.ToolCallState) {
	var id, name, display string
	var meta map[string]json.RawMessage
	switch c := call.Value.(type) {
	case *t.ToolCallStreamingState:
		id, name, display = c.ToolCallId, c.ToolName, c.DisplayName
		meta = c.Meta
	case *t.ToolCallRunningState:
		id, name, display = c.ToolCallId, c.ToolName, c.DisplayName
		meta = c.Meta
	case *t.ToolCallPendingConfirmationState:
		id, name, display = c.ToolCallId, c.ToolName, c.DisplayName
		meta = c.Meta
	case *t.ToolCallCompletedState:
		id, name, display = c.ToolCallId, c.ToolName, c.DisplayName
		meta = c.Meta
	default:
		a.chatAction(t.StateAction{Value: &t.ChatResponsePartAction{Type: t.ActionTypeChatResponsePart, TurnId: turn, Part: t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}}})
		return
	}
	a.chatAction(t.StateAction{Value: &t.ChatToolCallStartAction{Type: t.ActionTypeChatToolCallStart, TurnId: turn, ToolCallId: id, ToolName: name, DisplayName: display, Meta: meta}})
	switch c := call.Value.(type) {
	case *t.ToolCallPendingConfirmationState:
		a.chatAction(t.StateAction{Value: &t.ChatToolCallReadyAction{Type: t.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: id, InvocationMessage: c.InvocationMessage, ToolInput: c.ToolInput}})
	case *t.ToolCallStreamingState:
		if c.PartialInput != nil {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallDeltaAction{Type: t.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: id, Content: c.PartialInput}})
		}
	default:
		a.syncTool(turn, t.ToolCallState{Value: &t.ToolCallStreamingState{ToolCallId: id}}, call)
	}
}

func (a *ahpSession) syncTool(turn string, previous, next t.ToolCallState) bool {
	switch n := next.Value.(type) {
	case *t.ToolCallStreamingState:
		p, ok := previous.Value.(*t.ToolCallStreamingState)
		if !ok || p.ToolCallId != n.ToolCallId || p.PartialInput == nil || n.PartialInput == nil || !strings.HasPrefix(*n.PartialInput, *p.PartialInput) {
			return false
		}
		a.chatAction(t.StateAction{Value: &t.ChatToolCallDeltaAction{Type: t.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: n.ToolCallId, Content: ahpPtr(strings.TrimPrefix(*n.PartialInput, *p.PartialInput))}})
	case *t.ToolCallRunningState:
		if p, ok := previous.Value.(*t.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*t.ToolCallStreamingState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallReadyAction{Type: t.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: n.ToolCallId, InvocationMessage: n.InvocationMessage, ToolInput: n.ToolInput, Confirmed: &n.Confirmed}})
		} else {
			return false
		}
	case *t.ToolCallCompletedState:
		if p, ok := previous.Value.(*t.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*t.ToolCallStreamingState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallReadyAction{Type: t.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: n.ToolCallId, InvocationMessage: n.InvocationMessage, ToolInput: n.ToolInput, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*t.ToolCallRunningState); !ok || p.ToolCallId != n.ToolCallId {
			return false
		}
		a.chatAction(t.StateAction{Value: &t.ChatToolCallCompleteAction{Type: t.ActionTypeChatToolCallComplete, TurnId: turn, ToolCallId: n.ToolCallId, Result: t.ToolCallResult{Success: n.Success, PastTenseMessage: n.PastTenseMessage, Content: n.Content}}})
	case *t.ToolCallCancelledState:
		p, ok := previous.Value.(*t.ToolCallPendingConfirmationState)
		if !ok || p.ToolCallId != n.ToolCallId {
			return false
		}
		a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: false, Reason: &n.Reason}})
	default:
		return false
	}
	return true
}

func (a *ahpSession) resolvePromptLocked(command Command) {
	a.host.mu.Lock()
	defer a.host.mu.Unlock()
	if p := a.prompts[command.PromptID]; p != nil {
		p.response = &command
		if len(command.Content) > 0 {
			p.answers = ahpAnswerValues(p.view, command.Content)
		}
	}
}

func ahpPromptPart(p *ahpPrompt) t.ResponsePart {
	if p.view.Kind == PromptKindConfirm {
		id := "approval:" + p.view.ID
		message := t.NewStringOrMarkdownPlain(p.view.Message)
		var call t.ToolCallState
		if p.response == nil {
			call.Value = &t.ToolCallPendingConfirmationState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusPendingConfirmation, InvocationMessage: message}
		} else if p.response.Action == "accept" {
			call.Value = &t.ToolCallCompletedState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusCompleted, InvocationMessage: message, Confirmed: t.ToolCallConfirmationReasonUserAction, Success: true, PastTenseMessage: t.NewStringOrMarkdownPlain("Approved")}
		} else {
			call.Value = &t.ToolCallCancelledState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusCancelled, InvocationMessage: message, Reason: t.ToolCallCancellationReasonDenied}
		}
		return t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}
	}
	request := t.ChatInputRequest{Id: p.view.ID, Message: &p.view.Message, Questions: []t.ChatInputQuestion{}, Answers: p.answers}
	for _, f := range p.view.Fields {
		if f.CustomAnswerFor != "" {
			continue
		}
		message := f.Description
		if message == "" {
			message = f.Title
		}
		if message == "" {
			message = f.Name
		}
		var q t.ChatInputQuestion
		if len(f.Enum) > 0 {
			options := []t.ChatInputOption{}
			for i, v := range f.Enum {
				o := t.ChatInputOption{Id: v, Label: v}
				if i < len(f.EnumDescriptions) {
					o.Description = &f.EnumDescriptions[i]
				}
				options = append(options, o)
			}
			if f.Multiple {
				q.Value = &t.ChatInputMultiSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindMultiSelect, Options: options, AllowFreeformInput: ahpPtr(!f.Strict)}
			} else {
				q.Value = &t.ChatInputSingleSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindSingleSelect, Options: options, AllowFreeformInput: ahpPtr(!f.Strict)}
			}
		} else if f.Type == "boolean" {
			q.Value = &t.ChatInputBooleanQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindBoolean}
		} else if f.Type == "number" || f.Type == "integer" {
			q.Value = &t.ChatInputNumberQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindNumber}
		} else {
			q.Value = &t.ChatInputTextQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindText}
		}
		request.Questions = append(request.Questions, q)
	}
	part := &t.InputRequestResponsePart{Kind: t.ResponsePartKindInputRequest, Request: request}
	if p.response != nil {
		response := t.ChatInputResponseKind(p.response.Action)
		part.Response = &response
	}
	return t.ResponsePart{Value: part}
}

func (a *ahpSession) syncInputNeeded() {
	wanted := map[string]t.SessionInputRequest{}
	if active := a.chat.ActiveTurn; active != nil {
		for _, part := range active.ResponseParts {
			switch p := part.Value.(type) {
			case *t.InputRequestResponsePart:
				if p.Response == nil {
					wanted[p.Request.Id] = t.SessionInputRequest{Value: &t.SessionChatInputRequest{Id: p.Request.Id, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindChatInput, Request: p.Request}}
				}
			case *t.ToolCallResponsePart:
				if call, ok := p.ToolCall.Value.(*t.ToolCallPendingConfirmationState); ok {
					wanted[call.ToolCallId] = t.SessionInputRequest{Value: &t.SessionToolConfirmationRequest{Id: call.ToolCallId, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindToolConfirmation, TurnId: active.Id, ToolCall: t.ToolCallConfirmationState{Value: call}}}
				}
			}
		}
	}
	for _, request := range slices.Clone(a.state.InputNeeded) {
		id := ""
		switch r := request.Value.(type) {
		case *t.SessionChatInputRequest:
			id = r.Id
		case *t.SessionToolConfirmationRequest:
			id = r.Id
		}
		if _, ok := wanted[id]; !ok {
			a.sessionAction(t.StateAction{Value: &t.SessionInputNeededRemovedAction{Type: t.ActionTypeSessionInputNeededRemoved, Id: id}})
		}
	}
	for _, request := range wanted {
		if !slices.ContainsFunc(a.state.InputNeeded, func(old t.SessionInputRequest) bool { return reflect.DeepEqual(old, request) }) {
			a.sessionAction(t.StateAction{Value: &t.SessionInputNeededSetAction{Type: t.ActionTypeSessionInputNeededSet, Request: request}})
		}
	}
}

func ahpConfig(settings SessionSettings) *t.SessionConfigState {
	cfg := &t.SessionConfigState{Schema: t.SessionConfigSchema{Type: "object", Properties: map[string]t.SessionConfigPropertySchema{}}, Values: map[string]json.RawMessage{}}
	add := func(key, title, current string, values, labels []string) {
		if len(values) == 0 {
			return
		}
		def, _ := json.Marshal(current)
		property := t.SessionConfigPropertySchema{Type: "string", Title: title, Default: ahpPtr(json.RawMessage(def)), SessionMutable: ahpPtr(true), EnumLabels: labels}
		for _, value := range values {
			data, _ := json.Marshal(value)
			property.Enum = append(property.Enum, data)
		}
		cfg.Schema.Properties[key] = property
		cfg.Values[key] = def
	}
	models, names := []string{}, []string{}
	for _, m := range settings.Models {
		models = append(models, m.ID)
		names = append(names, m.Name)
	}
	add("model", "Model", settings.Model, models, names)
	add("effort", "Reasoning effort", settings.Effort, settings.Efforts, nil)
	modes, labels := []string{}, []string{}
	for _, m := range settings.Modes {
		modes = append(modes, m.ID)
		labels = append(labels, m.Name)
	}
	add("mode", "Mode", settings.Mode, modes, labels)
	if settings.Unattended != nil {
		value, _ := json.Marshal(*settings.Unattended)
		cfg.Schema.Properties["unattended"] = t.SessionConfigPropertySchema{Type: "boolean", Title: "Unattended", SessionMutable: ahpPtr(true), Default: ahpPtr(json.RawMessage(value))}
		cfg.Values["unattended"] = value
	}
	return cfg
}
