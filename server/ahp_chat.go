package server

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpChat projects one native session (its controller) as an AHP chat.
type ahpChat struct {
	host             *ahpHost
	session          *ahpSession
	controller       *sessionController
	chat             t.ChatState
	prompts          map[string]*ahpPrompt
	reasoning        map[string]*ahpReasoningTiming
	usageStart       UsageView // the controller's totals when the active turn started
	removed          bool
	projected        bool // turns created after the first projection are live, not history
	projectedInput   string
	projectedOutcome string
	projectedPrompts []PromptView
}

// The host is the producer of reasoning timing: a run starts at its first
// text and ends when the next content part starts or the turn ends.
type ahpReasoningTiming struct {
	started   time.Time
	startedAt string
	duration  *int64
}

type ahpPrompt struct {
	view     PromptView
	turn     string
	response *Command
	answers  map[string]t.ChatInputAnswer
}

// Approvals are answered once; the agent asks again for later operations.
var ahpApprovalOptions = []t.ConfirmationOption{
	{Id: "once", Label: "Allow", Kind: t.ConfirmationOptionKindApprove},
	{Id: "deny", Label: "Deny", Kind: t.ConfirmationOptionKindDeny},
}

// A retried dispatch carries its original origin; it is answered with the
// original acknowledgement instead of being applied again.
type ahpAck struct {
	key      string
	envelope t.ActionEnvelope
}

const ahpAckLimit = 512

func (a *ahpSession) remember(origin t.ActionOrigin, key string, envelope t.ActionEnvelope) {
	if _, exists := a.acks[origin]; !exists {
		a.ackOrder = append(a.ackOrder, origin)
		if len(a.ackOrder) > ahpAckLimit {
			delete(a.acks, a.ackOrder[0])
			a.ackOrder = a.ackOrder[1:]
		}
	}
	a.acks[origin] = ahpAck{key: key, envelope: envelope}
}

type ahpPendingAction struct {
	params   t.DispatchActionParams
	origin   t.ActionOrigin
	envelope *t.ActionEnvelope
}

func newAHPChat(a *ahpSession, c *sessionController, resource, title, stamp string) *ahpChat {
	return &ahpChat{host: a.host, session: a, controller: c, prompts: map[string]*ahpPrompt{}, reasoning: map[string]*ahpReasoningTiming{},
		chat: t.ChatState{Resource: resource, Title: title, Status: t.SessionStatusIdle, ModifiedAt: stamp, Turns: []t.Turn{}, Config: &t.ChatConfigState{Values: ahpConfig(c.state.Settings).Values}},
	}
}

// emit publishes an action, acknowledging the client action it completes.
func (a *ahpSession) emit(channel string, action t.StateAction) {
	var origin *t.ActionOrigin
	p := a.pending[ahpActionKey(action)]
	if p != nil && p.envelope == nil && channel == p.params.Channel {
		origin = &p.origin
	}
	envelope := a.host.publish(channel, action, origin, nil)
	if origin != nil {
		p.envelope = &envelope
		a.remember(*origin, ahpActionKey(action), envelope)
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

func (a *ahpChat) chatAction(action t.StateAction) {
	ahp.ApplyActionToChat(&a.chat, action)
	a.session.emit(a.chat.Resource, action)
}

func (a *ahpChat) syncLocked(c *sessionController, changes []SessionChange) {
	a.host.mu.Lock()
	defer a.host.mu.Unlock()
	if a.session.deleted || a.removed {
		return
	}
	if c.state.Status == "deleted" {
		a.session.chatDeleted(a)
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
					a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: a.chat.ActiveTurn.Id, PartId: p.Id, Content: change.Text, StartedAt: a.startReasoning(p.Id, change.Text)}})
					return
				}
			}
		}
	}
	// Updates of a running tool change only its own part.
	if len(changes) == 1 && changes[0].Type == "entry.upsert" && changes[0].Entry != nil && changes[0].Entry.Type == "tool" && a.chat.ActiveTurn != nil {
		part, _ := a.host.entryPart(*changes[0].Entry)
		next := part.Value.(*t.ToolCallResponsePart).ToolCall
		for _, p := range a.chat.ActiveTurn.ResponseParts {
			if tool, ok := p.Value.(*t.ToolCallResponsePart); ok && ahpToolCallID(tool.ToolCall) == ahpToolCallID(next) {
				if reflect.DeepEqual(tool.ToolCall, next) || a.syncTool(a.chat.ActiveTurn.Id, tool.ToolCall, next) {
					return
				}
				break
			}
		}
	}
	a.syncProjection(c, changes)
}

func ahpToolCallID(call t.ToolCallState) string {
	switch c := call.Value.(type) {
	case *t.ToolCallStreamingState:
		return c.ToolCallId
	case *t.ToolCallRunningState:
		return c.ToolCallId
	case *t.ToolCallPendingConfirmationState:
		return c.ToolCallId
	case *t.ToolCallCompletedState:
		return c.ToolCallId
	case *t.ToolCallCancelledState:
		return c.ToolCallId
	}
	return ""
}

func (a *ahpChat) syncProjection(c *sessionController, changes []SessionChange) {
	previousSummary := a.session.summary()
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
		if old := a.chat.ActiveTurn; old != nil && c.activeInput != old.Id {
			a.closeReasoning() // final timing precedes the turn's terminal action
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
				started := time.Now().UTC().Format(time.RFC3339Nano)
				if last.StartedAt != nil {
					started = *last.StartedAt
				}
				a.chatAction(t.StateAction{Value: &t.ChatTurnStartedAction{Type: t.ActionTypeChatTurnStarted, TurnId: last.Id, StartedAt: started, Message: last.Message}})
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
				a.usageStart = c.state.Usage
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
	a.syncConfig(c)
	a.syncQueue(c)
	a.syncUsage(c)
	a.syncActivity(c)
	a.syncTitle(c)
	a.session.syncChats(previousSummary)
	a.projected = true
}

func (a *ahpChat) finishTurn(id string, state t.TurnState, message *string) {
	var duration int64
	if active := a.chat.ActiveTurn; active != nil && active.Id == id {
		if started, err := time.Parse(time.RFC3339Nano, active.StartedAt); err == nil {
			duration = max(0, time.Since(started).Milliseconds())
		}
	}
	if state != t.TurnStateCancelled {
		// A cancel that lost the race to the turn's end must still be answered.
		if p := a.session.pending[ahpActionKey(t.StateAction{Value: &t.ChatTurnCancelledAction{TurnId: id}})]; p != nil && p.envelope == nil {
			reason := "the turn finished before it could be cancelled"
			envelope := a.host.publish(p.params.Channel, p.params.Action, &p.origin, &reason)
			p.envelope = &envelope
			a.session.remember(p.origin, ahpActionKey(p.params.Action), envelope)
			delete(a.session.pending, ahpActionKey(p.params.Action))
		}
	}
	switch state {
	case t.TurnStateCancelled:
		a.chatAction(t.StateAction{Value: &t.ChatTurnCancelledAction{Type: t.ActionTypeChatTurnCancelled, TurnId: id, Duration: duration}})
	case t.TurnStateError:
		text := "Turn failed"
		if message != nil {
			text = *message
		}
		a.chatAction(t.StateAction{Value: &t.ChatErrorAction{Type: t.ActionTypeChatError, TurnId: id, Duration: duration, Part: t.ErrorResponsePart{Kind: t.ResponsePartKindError, Error: t.ErrorInfo{ErrorType: "agent", Message: text}}}})
	default:
		a.chatAction(t.StateAction{Value: &t.ChatTurnCompleteAction{Type: t.ActionTypeChatTurnComplete, TurnId: id, Duration: duration}})
	}
}

func (a *ahpChat) transcript(c *sessionController) ([]t.Turn, *t.ActiveTurn) {
	old := append([]t.Turn{}, a.chat.Turns...)
	if active := a.chat.ActiveTurn; active != nil {
		old = append(old, t.Turn{Id: active.Id, StartedAt: &active.StartedAt, Message: active.Message, ResponseParts: active.ResponseParts, Usage: active.Usage, State: t.TurnStateComplete})
	}
	turns := []t.Turn{}
	seen := map[string]bool{}
	for _, e := range c.entries {
		if e.Type == "user" {
			if c.activeInput != "" && len(turns) > 0 && turns[len(turns)-1].Id == c.activeInput && e.InputID != c.activeInput {
				last := &turns[len(turns)-1]
				last.ResponseParts = append(last.ResponseParts, t.ResponsePart{Value: &t.SystemNotificationResponsePart{Kind: t.ResponsePartKindSystemNotification, Content: t.NewStringOrMarkdownPlain(e.Content)}})
				continue
			}
			id := e.InputID
			if id == "" {
				id = e.ID
			}
			var startedAt, duration = (*string)(nil), (*int64)(nil)
			var usage *t.UsageInfo
			message := a.host.message(e.Content, e.Files, e.Images)
			if pending := a.session.pending["start:"+id]; pending != nil {
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
					startedAt, duration, usage = prior.StartedAt, prior.Duration, prior.Usage
					message = prior.Message
					message.Text = e.Content
				}
			} else if a.projected {
				startedAt = ahpPtr(time.Now().UTC().Format(time.RFC3339Nano))
			}
			turns = append(turns, t.Turn{Id: id, StartedAt: startedAt, Duration: duration, Usage: usage, Message: message, ResponseParts: []t.ResponsePart{}, State: t.TurnStateComplete})
			continue
		}
		if len(turns) == 0 {
			continue
		}
		if part, ok := a.host.entryPart(e); ok {
			if p, ok := part.Value.(*t.ReasoningResponsePart); ok {
				seen[p.Id] = true
				if timing := a.reasoning[p.Id]; timing != nil {
					p.StartedAt, p.Duration = &timing.startedAt, timing.duration
				}
			}
			last := &turns[len(turns)-1]
			last.ResponseParts = append(last.ResponseParts, part)
		}
	}
	for id := range a.reasoning {
		if !seen[id] {
			delete(a.reasoning, id)
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
		started := time.Now().UTC().Format(time.RFC3339Nano)
		if last.StartedAt != nil {
			started = *last.StartedAt
		}
		return turns[:len(turns)-1], &t.ActiveTurn{Id: last.Id, StartedAt: started, Message: last.Message, ResponseParts: last.ResponseParts}
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

func (h *ahpHost) entryPart(e TranscriptEntry) (t.ResponsePart, bool) {
	switch e.Type {
	case "assistant":
		content := e.Content
		for _, image := range e.Images {
			content += "\n\n![Image](" + image + ")"
		}
		return t.ResponsePart{Value: &t.MarkdownResponsePart{Kind: t.ResponsePartKindMarkdown, Id: e.ID, Content: content}}, true
	case "reasoning":
		return t.ResponsePart{Value: &t.ReasoningResponsePart{Kind: t.ResponsePartKindReasoning, Id: e.ID, Content: e.Content}}, true
	case "notice":
		return t.ResponsePart{Value: &t.SystemNotificationResponsePart{Kind: t.ResponsePartKindSystemNotification, Content: t.NewStringOrMarkdownPlain(e.Content)}}, true
	case "tool":
		id := e.ToolID
		if id == "" {
			id = e.ID
		}
		// VS Code's presentation convention for tool kinds.
		var meta map[string]json.RawMessage
		if kind := e.ToolKind; kind != "" {
			if kind == "execute" {
				kind = "terminal"
			}
			data, _ := json.Marshal(kind)
			meta = map[string]json.RawMessage{"toolKind": data}
		}
		var intention *string
		if e.ToolHint != "" {
			intention = &e.ToolHint
		}
		var content []t.ToolResultContent
		if e.ToolResult != nil {
			content = append(content, t.ToolResultContent{Value: &t.ToolResultTextContent{Type: t.ToolResultContentTypeText, Text: *e.ToolResult}})
		}
		for _, location := range e.ToolLocations {
			uri := h.fileURI(location.Path)
			if location.Line > 0 {
				uri += "#L" + strconv.Itoa(location.Line)
			}
			content = append(content, t.ToolResultContent{Value: &t.ToolResultResourceContent{Type: t.ToolResultContentTypeResource, Uri: uri}})
		}
		for _, image := range e.Images {
			prefix, data, ok := strings.Cut(image, ",")
			if ok && strings.HasPrefix(prefix, "data:image/") && strings.HasSuffix(prefix, ";base64") {
				content = append(content, t.ToolResultContent{Value: &t.ToolResultEmbeddedResourceContent{Type: t.ToolResultContentTypeEmbeddedResource, ContentType: strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64"), Data: data}})
			}
		}
		invocation := t.NewStringOrMarkdownPlain(e.ToolName)
		input := &t.ToolInput{Inline: &e.ToolArgs}
		var call t.ToolCallState
		if e.ToolResult != nil {
			call.Value = &t.ToolCallCompletedState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: t.ToolCallStatusCompleted, InvocationMessage: invocation, ToolInput: input, Confirmed: t.ToolCallConfirmationReasonNotNeeded, Success: true, PastTenseMessage: invocation, Content: content}
		} else if e.ToolPartial {
			call.Value = &t.ToolCallStreamingState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: t.ToolCallStatusStreaming, PartialInput: &e.ToolArgs}
		} else {
			call.Value = &t.ToolCallRunningState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: t.ToolCallStatusRunning, InvocationMessage: invocation, ToolInput: input, Confirmed: t.ToolCallConfirmationReasonNotNeeded, Content: content}
		}
		return t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}, true
	}
	return t.ResponsePart{}, false
}

func (a *ahpChat) syncParts(parts []t.ResponsePart) bool {
	active := a.chat.ActiveTurn
	if len(active.ResponseParts) > len(parts) {
		return false
	}
	for i, next := range parts {
		if i >= len(active.ResponseParts) {
			switch p := next.Value.(type) {
			case *t.MarkdownResponsePart, *t.ToolCallResponsePart:
				a.closeReasoning()
			case *t.ReasoningResponsePart:
				a.closeReasoning()
				if stamp := a.startReasoning(p.Id, p.Content); stamp != nil {
					p.StartedAt = stamp
				}
			}
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
			a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: active.Id, PartId: n.Id, Content: strings.TrimPrefix(n.Content, p.Content), StartedAt: a.startReasoning(n.Id, n.Content)}})
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

func (a *ahpChat) startReasoning(id, content string) *string {
	if content == "" || a.reasoning[id] != nil {
		return nil
	}
	now := time.Now()
	timing := &ahpReasoningTiming{started: now, startedAt: now.UTC().Format(time.RFC3339Nano)}
	a.reasoning[id] = timing
	return &timing.startedAt
}

func (a *ahpChat) closeReasoning() {
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	for _, part := range active.ResponseParts {
		p, ok := part.Value.(*t.ReasoningResponsePart)
		if !ok {
			continue
		}
		timing := a.reasoning[p.Id]
		if timing == nil || timing.duration != nil {
			continue
		}
		timing.duration = ahpPtr(max(0, time.Since(timing.started).Milliseconds()))
		a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: active.Id, PartId: p.Id, Duration: timing.duration}})
	}
}

func (a *ahpChat) appendTool(turn string, call t.ToolCallState) {
	var id, name, display string
	var intention *string
	var meta map[string]json.RawMessage
	switch c := call.Value.(type) {
	case *t.ToolCallStreamingState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta = c.Meta
	case *t.ToolCallRunningState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta = c.Meta
	case *t.ToolCallPendingConfirmationState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta = c.Meta
	case *t.ToolCallCompletedState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta = c.Meta
	default:
		a.chatAction(t.StateAction{Value: &t.ChatResponsePartAction{Type: t.ActionTypeChatResponsePart, TurnId: turn, Part: t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}}})
		return
	}
	a.chatAction(t.StateAction{Value: &t.ChatToolCallStartAction{Type: t.ActionTypeChatToolCallStart, TurnId: turn, ToolCallId: id, ToolName: name, DisplayName: display, Intention: intention, Meta: meta}})
	switch c := call.Value.(type) {
	case *t.ToolCallPendingConfirmationState:
		a.chatAction(t.StateAction{Value: &t.ChatToolCallReadyAction{Type: t.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: id, InvocationMessage: c.InvocationMessage, ToolInput: c.ToolInput, Options: c.Options}})
	case *t.ToolCallStreamingState:
		if c.PartialInput != nil {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallDeltaAction{Type: t.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: id, Content: c.PartialInput}})
		}
	default:
		a.syncTool(turn, t.ToolCallState{Value: &t.ToolCallStreamingState{ToolCallId: id}}, call)
	}
}

func (a *ahpChat) syncTool(turn string, previous, next t.ToolCallState) bool {
	switch n := next.Value.(type) {
	case *t.ToolCallStreamingState:
		p, ok := previous.Value.(*t.ToolCallStreamingState)
		if !ok || p.ToolCallId != n.ToolCallId || p.PartialInput == nil || n.PartialInput == nil || !strings.HasPrefix(*n.PartialInput, *p.PartialInput) {
			return false
		}
		a.chatAction(t.StateAction{Value: &t.ChatToolCallDeltaAction{Type: t.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: n.ToolCallId, Content: ahpPtr(strings.TrimPrefix(*n.PartialInput, *p.PartialInput))}})
	case *t.ToolCallRunningState:
		var content []t.ToolResultContent
		if p, ok := previous.Value.(*t.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed, SelectedOptionId: ahpOptionID(n.SelectedOption)}})
		} else if p, ok := previous.Value.(*t.ToolCallStreamingState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallReadyAction{Type: t.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: n.ToolCallId, InvocationMessage: n.InvocationMessage, ToolInput: n.ToolInput, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*t.ToolCallRunningState); ok && p.ToolCallId == n.ToolCallId {
			content = p.Content
		} else {
			return false
		}
		if !reflect.DeepEqual(content, n.Content) {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallContentChangedAction{Type: t.ActionTypeChatToolCallContentChanged, TurnId: turn, ToolCallId: n.ToolCallId, Content: n.Content}})
		}
	case *t.ToolCallCompletedState:
		if p, ok := previous.Value.(*t.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed, SelectedOptionId: ahpOptionID(n.SelectedOption)}})
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
		a.chatAction(t.StateAction{Value: &t.ChatToolCallConfirmedAction{Type: t.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: false, Reason: &n.Reason, SelectedOptionId: ahpOptionID(n.SelectedOption)}})
	default:
		return false
	}
	return true
}

func ahpOptionID(option *t.ConfirmationOption) *string {
	if option == nil {
		return nil
	}
	return &option.Id
}

func (a *ahpChat) resolvePromptLocked(command Command) {
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
			call.Value = &t.ToolCallPendingConfirmationState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusPendingConfirmation, InvocationMessage: message, Options: ahpApprovalOptions}
		} else if p.response.Action == "accept" {
			call.Value = &t.ToolCallCompletedState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusCompleted, InvocationMessage: message, Confirmed: t.ToolCallConfirmationReasonUserAction, SelectedOption: p.response.option, Success: true, PastTenseMessage: t.NewStringOrMarkdownPlain("Approved")}
		} else {
			call.Value = &t.ToolCallCancelledState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: t.ToolCallStatusCancelled, InvocationMessage: message, Reason: t.ToolCallCancellationReasonDenied, SelectedOption: p.response.option}
		}
		return t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}
	}
	request := t.ChatInputRequest{Id: p.view.ID, Message: &p.view.Message, Questions: []t.ChatInputQuestion{}, Answers: p.answers}
	companions := map[string]bool{}
	for _, f := range p.view.Fields {
		if f.CustomAnswerFor != "" {
			companions[f.CustomAnswerFor] = true
		}
	}
	for _, f := range p.view.Fields {
		if f.CustomAnswerFor != "" {
			continue // offered as its enum field's free-form answer
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
				if promptDefaultIncludes(f.Default, v) {
					o.Recommended = ahpPtr(true)
				}
				options = append(options, o)
			}
			freeform := ahpPtr(!f.Strict || companions[f.Name])
			if f.Multiple {
				q.Value = &t.ChatInputMultiSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindMultiSelect, Options: options, AllowFreeformInput: freeform}
			} else {
				q.Value = &t.ChatInputSingleSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindSingleSelect, Options: options, AllowFreeformInput: freeform}
			}
		} else if f.Type == "boolean" {
			question := &t.ChatInputBooleanQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindBoolean}
			if value, ok := f.Default.(bool); ok {
				question.DefaultValue = &value
			}
			q.Value = question
		} else if f.Type == "number" || f.Type == "integer" {
			question := &t.ChatInputNumberQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindNumber}
			if value, ok := f.Default.(float64); ok {
				question.DefaultValue = &value
			}
			q.Value = question
		} else {
			question := &t.ChatInputTextQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: t.ChatInputQuestionKindText}
			if value, ok := f.Default.(string); ok {
				question.DefaultValue = &value
			}
			q.Value = question
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

func promptDefaultIncludes(value any, option string) bool {
	switch v := value.(type) {
	case string:
		return v == option
	case []any:
		return slices.Contains(v, any(option))
	case []string:
		return slices.Contains(v, option)
	}
	return false
}

func ahpConfig(settings SessionSettings) *t.SessionConfigState {
	cfg := &t.SessionConfigState{Schema: t.SessionConfigSchema{Type: "object", Properties: map[string]t.SessionConfigPropertySchema{}}, Values: map[string]json.RawMessage{}}
	add := func(key, title, current string, values, labels []string) {
		if len(values) == 0 {
			return
		}
		def, _ := json.Marshal(current)
		property := t.SessionConfigPropertySchema{Type: "string", Title: title, Default: ahpPtr(json.RawMessage(def)), SessionMutable: ahpPtr(true), ChatMutable: ahpPtr(true), EnumLabels: labels}
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
	// A configured model stays selectable while discovery has listed none.
	if len(models) == 0 && settings.Model != "" {
		models, names = []string{settings.Model}, []string{settings.Model}
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
		cfg.Schema.Properties["unattended"] = t.SessionConfigPropertySchema{Type: "boolean", Title: "Unattended", SessionMutable: ahpPtr(true), ChatMutable: ahpPtr(true), Default: ahpPtr(json.RawMessage(value))}
		cfg.Values["unattended"] = value
	}
	return cfg
}
