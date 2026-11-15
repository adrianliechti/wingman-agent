package server

import (
	"encoding/json"
	"maps"
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
	project          *ahpProject
	session          *ahpSession
	controller       *sessionController
	chat             t.ChatState
	title            string                     // the session's name for the chat, which no action can give the chat itself
	config           *t.SessionConfigState      // the controller's, read without its lock
	own              map[string]json.RawMessage // the config a client set on this chat, which it keeps over the session's
	prompts          map[string]*ahpPrompt
	promptSeq        int
	reasoning        map[string]*ahpReasoningTiming
	endings          map[string]t.StateAction // the action that ended each turn the host ended
	usageStart       UsageView                // the controller's totals when the active turn started
	offered          map[string]string        // the client behind each tool the agent was last given
	clientCalls      map[string]*ahpClientCall
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
	turn     string // empty when no turn was active
	after    string // the entry it follows in its turn
	seq      int
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

// ahpPendingAction is a client's action that the host has not answered yet.
type ahpPendingAction struct {
	params t.DispatchActionParams
	origin t.ActionOrigin
}

func newAHPChat(a *ahpSession, c *sessionController, resource, title, stamp string) *ahpChat {
	config := ahpConfig(c.state.Settings)
	return &ahpChat{project: a.project, session: a, controller: c, title: title, config: config, prompts: map[string]*ahpPrompt{}, reasoning: map[string]*ahpReasoningTiming{}, clientCalls: map[string]*ahpClientCall{}, endings: map[string]t.StateAction{}, own: map[string]json.RawMessage{},
		chat: t.ChatState{Resource: resource, Title: title, Status: t.SessionStatusIdle, ModifiedAt: stamp, Turns: []t.Turn{}, Config: &t.ChatConfigState{Values: map[string]json.RawMessage{}}},
	}
}

// emit publishes an action, acknowledging the client action it completes.
func (a *ahpSession) emit(channel string, action t.StateAction) {
	if !a.answer(ahpActionKey(channel, action), action, nil) {
		a.project.publish(channel, action, nil, nil)
	}
}

// acknowledge answers a client's action with the host's own, such as the
// values a backend normalized.
func (a *ahpSession) acknowledge(channel string, client, action t.StateAction) {
	a.answer(ahpActionKey(channel, client), action, nil)
}

// answer publishes the host's answer to a pending client action: the action
// that completes it, or, without one, the client's own, rejected when a
// reason is given. It reports whether the action was pending. Caller holds host.mu.
func (a *ahpSession) answer(key string, action t.StateAction, rejection *string) bool {
	p := a.pending[key]
	if p == nil {
		return false
	}
	delete(a.pending, key)
	if action.Value == nil {
		action = p.params.Action
	}
	a.remember(p.origin, key, a.project.publish(p.params.Channel, action, &p.origin, rejection))
	return true
}

// ahpActionKey identifies an action on a channel: the host's action that
// completes a client's has the client's key.
func ahpActionKey(channel string, action t.StateAction) string {
	switch v := action.Value.(type) {
	case *t.ChatPendingMessageSetAction:
		return channel + " pending-set:" + v.Id
	case *t.ChatPendingMessageRemovedAction:
		return channel + " pending-remove:" + v.Id
	case *t.ChatTurnStartedAction:
		return channel + " start:" + v.TurnId
	case *t.ChatTurnCancelledAction:
		return channel + " cancel:" + v.TurnId
	case *t.ChatToolCallConfirmedAction:
		return channel + " confirm:" + v.ToolCallId
	case *t.ChatInputCompletedAction:
		return channel + " input:" + v.RequestId
	default:
		data, _ := json.Marshal(action)
		return channel + " " + string(data)
	}
}

func (a *ahpChat) chatAction(action t.StateAction) {
	ahp.ApplyActionToChat(&a.chat, action)
	a.session.emit(a.chat.Resource, action)
}

func (a *ahpChat) syncLocked(c *sessionController, changes []SessionChange) {
	a.project.mu.Lock()
	defer func() {
		unsaved := a.session.unsaved
		a.project.mu.Unlock()
		if unsaved {
			a.session.save()
		}
	}()
	if !a.project.availableLocked() || a.session.deleted || a.removed {
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
		part, _ := a.entryPart(*changes[0].Entry)
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
				// A prompt joins the running turn, unless another input asks, such as a background task's.
				turn := c.activeInput
				if a.chat.ActiveTurn != nil {
					turn = a.chat.ActiveTurn.Id
				}
				if p.InputID != "" && p.InputID != c.activeInput && c.steered[p.InputID] != c.activeInput {
					turn = ""
				}
				after := ""
				if n := len(c.entries); n > 0 {
					after = c.entries[n-1].ID
				}
				a.promptSeq++
				a.prompts[p.ID] = &ahpPrompt{view: p, turn: turn, after: after, seq: a.promptSeq}
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
			// A prompt a completed turn left open was not its own; it stays answerable as the session's.
			for _, p := range a.prompts {
				if state == t.TurnStateComplete && p.turn == old.Id && p.response == nil {
					p.turn = ""
				}
			}
			a.finishTurn(old.Id, state, c.state.Error)
			// The finished turn now carries its duration.
			turns, active = a.transcript(c)
		}
		// A client's cut leaves the turns the agent kept.
		if active == nil && !reflect.DeepEqual(a.chat.Turns, turns) {
			for _, p := range a.session.pending {
				if _, cut := p.params.Action.Value.(*t.ChatTruncatedAction); cut && p.params.Channel == a.chat.Resource {
					a.chatAction(p.params.Action)
					break
				}
			}
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
				a.startTurn(active)
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

// startTurn announces a new turn. A turn that consumes a pending message
// shows the message leaving the queue first.
func (a *ahpChat) startTurn(active *t.ActiveTurn) {
	started := &t.ChatTurnStartedAction{Type: t.ActionTypeChatTurnStarted, TurnId: active.Id, StartedAt: active.StartedAt, Message: active.Message}
	// An idle chat consumes a message before its queue ever held it.
	if p := a.session.pending[ahpActionKey(a.chat.Resource, t.StateAction{Value: &t.ChatPendingMessageSetAction{Id: active.Id}})]; p != nil {
		a.chatAction(p.params.Action)
	}
	if i := slices.IndexFunc(a.chat.QueuedMessages, func(m t.PendingMessage) bool { return m.Id == active.Id }); i >= 0 {
		kind := t.PendingMessageKindQueued
		if k := a.chat.QueuedMessages[i].Kind; k != nil {
			kind = *k
		}
		a.chatAction(t.StateAction{Value: &t.ChatPendingMessageRemovedAction{Type: t.ActionTypeChatPendingMessageRemoved, Kind: kind, Id: active.Id}})
		started.QueuedMessageId = &active.Id
	}
	a.chatAction(t.StateAction{Value: started})
}

func (a *ahpChat) finishTurn(id string, state t.TurnState, message *string) {
	var duration int64
	if active := a.chat.ActiveTurn; active != nil && active.Id == id {
		if started, err := time.Parse(time.RFC3339Nano, active.StartedAt); err == nil {
			duration = max(0, time.Since(started).Milliseconds())
		}
	}
	ending := t.StateAction{Value: &t.ChatTurnCompleteAction{Type: t.ActionTypeChatTurnComplete, TurnId: id, Duration: duration}}
	switch state {
	case t.TurnStateCancelled:
		ending = t.StateAction{Value: &t.ChatTurnCancelledAction{Type: t.ActionTypeChatTurnCancelled, TurnId: id, Duration: duration}}
	case t.TurnStateError:
		text := "Turn failed"
		if message != nil {
			text = *message
		}
		ending = t.StateAction{Value: &t.ChatErrorAction{Type: t.ActionTypeChatError, TurnId: id, Duration: duration, Part: t.ErrorResponsePart{Kind: t.ResponsePartKindError, Error: t.ErrorInfo{ErrorType: "agent", Message: text}}}}
	}
	a.endings[id] = ending
	a.chatAction(ending)
	if state == t.TurnStateCancelled {
		return
	}
	// A cancel that lost the race is answered once its client can see the turn ended.
	key := ahpActionKey(a.chat.Resource, t.StateAction{Value: &t.ChatTurnCancelledAction{TurnId: id}})
	a.session.answer(key, t.StateAction{}, new("the turn finished before it could be cancelled"))
}

func (a *ahpChat) transcript(c *sessionController) ([]t.Turn, *t.ActiveTurn) {
	old := append([]t.Turn{}, a.chat.Turns...)
	if active := a.chat.ActiveTurn; active != nil {
		old = append(old, t.Turn{Id: active.Id, StartedAt: &active.StartedAt, Message: active.Message, ResponseParts: active.ResponseParts, Usage: active.Usage, State: t.TurnStateComplete})
	}
	turns := []t.Turn{}
	anchors := [][]string{} // per turn: its user entry, then the entry of each part
	seen := map[string]bool{}
	for _, e := range c.entries {
		if e.Type == "user" {
			// Steering stays in the turn it joined; while a turn runs, any other input joins it.
			joined := c.steered[e.InputID]
			if joined == "" && e.InputID != c.activeInput {
				joined = c.activeInput
			}
			if joined != "" && len(turns) > 0 && turns[len(turns)-1].Id == joined {
				last := &turns[len(turns)-1]
				last.ResponseParts = append(last.ResponseParts, t.ResponsePart{Value: &t.SystemNotificationResponsePart{Kind: t.ResponsePartKindSystemNotification, Content: t.NewStringOrMarkdownPlain(e.Content)}})
				anchors[len(anchors)-1] = append(anchors[len(anchors)-1], e.ID)
				continue
			}
			id := e.InputID
			if id == "" {
				id = e.ID
			}
			var startedAt, duration = (*string)(nil), (*int64)(nil)
			var usage *t.UsageInfo
			message := a.project.message(e.Source, e.Content, e.Files, e.Images)
			if e.Source == nil && e.Content == "" && len(e.Files)+len(e.Images) == 0 {
				message.Origin.Kind = t.MessageKindSystemNotification // such as a background task's result
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
				startedAt = new(time.Now().UTC().Format(time.RFC3339Nano))
			}
			turns = append(turns, t.Turn{Id: id, StartedAt: startedAt, Duration: duration, Usage: usage, Message: message, ResponseParts: []t.ResponsePart{}, State: t.TurnStateComplete})
			anchors = append(anchors, []string{e.ID})
			continue
		}
		if len(turns) == 0 {
			continue
		}
		if part, ok := a.entryPart(e); ok {
			if p, ok := part.Value.(*t.ReasoningResponsePart); ok {
				seen[p.Id] = true
				if timing := a.reasoning[p.Id]; timing != nil {
					p.StartedAt, p.Duration = &timing.startedAt, timing.duration
				}
			}
			last := &turns[len(turns)-1]
			last.ResponseParts = append(last.ResponseParts, part)
			anchors[len(anchors)-1] = append(anchors[len(anchors)-1], e.ID)
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
		prompts := []*ahpPrompt{}
		for _, p := range a.prompts {
			if p.turn == turn.Id {
				prompts = append(prompts, p)
			}
		}
		slices.SortFunc(prompts, func(x, y *ahpPrompt) int { return x.seq - y.seq })
		// A prompt stays after the entry it followed, so later parts append to the turn.
		for _, p := range prompts {
			at := len(anchors[i]) - 1
			for at >= 0 && anchors[i][at] != p.after {
				at--
			}
			if at < 0 {
				at = len(anchors[i]) - 1
			}
			turn.ResponseParts = slices.Insert(turn.ResponseParts, at, ahpPromptPart(p))
			anchors[i] = slices.Insert(anchors[i], at+1, p.after)
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
		return a.ended(turns[:len(turns)-1]), &t.ActiveTurn{Id: last.Id, StartedAt: started, Message: last.Message, ResponseParts: last.ResponseParts}
	}
	switch code.TurnInputState(c.lastOutcome) {
	case code.TurnInputCancelled:
		last.State = t.TurnStateCancelled
	case code.TurnInputFailed:
		last.State = t.TurnStateError
	}
	// A failed turn restored from history ends with the session's error.
	if _, ended := a.endings[last.Id]; !ended && last.State == t.TurnStateError {
		message := "Turn failed"
		if c.state.Error != nil {
			message = *c.state.Error
		}
		last.ResponseParts = append(last.ResponseParts, t.ResponsePart{Value: &t.ErrorResponsePart{Kind: t.ResponsePartKindError, Error: t.ErrorInfo{ErrorType: "agent", Message: message}}})
	}
	return a.ended(turns), nil
}

// ended finishes the turns the host ended the way the reducer finished them for its clients.
func (a *ahpChat) ended(turns []t.Turn) []t.Turn {
	ids := map[string]bool{}
	for i, turn := range turns {
		ids[turn.Id] = true
		ending, ok := a.endings[turn.Id]
		if !ok {
			continue
		}
		active := &t.ActiveTurn{Id: turn.Id, Message: turn.Message, ResponseParts: turn.ResponseParts, Usage: turn.Usage}
		if turn.StartedAt != nil {
			active.StartedAt = *turn.StartedAt
		}
		scratch := t.ChatState{ActiveTurn: active, Turns: []t.Turn{}}
		ahp.ApplyActionToChat(&scratch, ending)
		if len(scratch.Turns) == 1 {
			turns[i] = scratch.Turns[0]
		}
	}
	for id := range a.endings {
		if !ids[id] {
			delete(a.endings, id)
		}
	}
	return turns
}

func (p *ahpProject) entryPart(e TranscriptEntry) (t.ResponsePart, bool) {
	switch e.Type {
	case "assistant":
		var content strings.Builder
		content.WriteString(e.Content)
		for _, image := range e.Images {
			content.WriteString("\n\n![Image](" + image + ")")
		}
		return t.ResponsePart{Value: &t.MarkdownResponsePart{Kind: t.ResponsePartKindMarkdown, Id: e.ID, Content: content.String()}}, true
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
			uri := p.fileURI(location.Path)
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
		timing.duration = new(max(0, time.Since(timing.started).Milliseconds()))
		a.chatAction(t.StateAction{Value: &t.ChatReasoningAction{Type: t.ActionTypeChatReasoning, TurnId: active.Id, PartId: p.Id, Duration: timing.duration}})
	}
}

func (a *ahpChat) appendTool(turn string, call t.ToolCallState) {
	var id, name, display string
	var intention *string
	var meta map[string]json.RawMessage
	var contributor *t.ToolCallContributor
	switch c := call.Value.(type) {
	case *t.ToolCallStreamingState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *t.ToolCallRunningState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *t.ToolCallPendingConfirmationState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *t.ToolCallCompletedState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	default:
		a.chatAction(t.StateAction{Value: &t.ChatResponsePartAction{Type: t.ActionTypeChatResponsePart, TurnId: turn, Part: t.ResponsePart{Value: &t.ToolCallResponsePart{Kind: t.ResponsePartKindToolCall, ToolCall: call}}}})
		return
	}
	a.chatAction(t.StateAction{Value: &t.ChatToolCallStartAction{Type: t.ActionTypeChatToolCallStart, TurnId: turn, ToolCallId: id, ToolName: name, DisplayName: display, Intention: intention, Meta: meta, Contributor: contributor}})
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
		a.chatAction(t.StateAction{Value: &t.ChatToolCallDeltaAction{Type: t.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: n.ToolCallId, Content: new(strings.TrimPrefix(*n.PartialInput, *p.PartialInput))}})
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
		a.chatAction(t.StateAction{Value: &t.ChatToolCallCompleteAction{Type: t.ActionTypeChatToolCallComplete, TurnId: turn, ToolCallId: n.ToolCallId, Result: t.ToolCallResult{Success: n.Success, PastTenseMessage: n.PastTenseMessage, Content: n.Content, StructuredContent: n.StructuredContent, ResultMetadata: n.ResultMetadata, Error: n.Error}}})
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
	a.project.mu.Lock()
	defer a.project.mu.Unlock()
	if p := a.prompts[command.PromptID]; p != nil {
		p.response = &command
		switch {
		case command.answers != nil:
			p.answers = command.answers
		case len(command.Content) > 0: // answered by a client of another protocol
			p.answers = ahpAnswerValues(p.view, command.Content)
		}
		if len(p.answers) == 0 {
			p.answers = nil
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
					o.Recommended = new(true)
				}
				options = append(options, o)
			}
			freeform := new(!f.Strict || companions[f.Name])
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

// ahpPromptRequest is a prompt as an input request; an approval has no questions.
func ahpPromptRequest(p *ahpPrompt) t.ChatInputRequest {
	if p.view.Kind == PromptKindConfirm {
		return t.ChatInputRequest{Id: p.view.ID, Message: &p.view.Message, Questions: []t.ChatInputQuestion{}}
	}
	return ahpPromptPart(p).Value.(*t.InputRequestResponsePart).Request
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
		// A default that followed the value would change the schema without an action.
		property := t.SessionConfigPropertySchema{Type: "string", Title: title, SessionMutable: new(true), ChatMutable: new(true), EnumLabels: labels}
		for _, value := range values {
			data, _ := json.Marshal(value)
			property.Enum = append(property.Enum, data)
		}
		cfg.Schema.Properties[key] = property
		cfg.Values[key], _ = json.Marshal(current)
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
		cfg.Schema.Properties["unattended"] = t.SessionConfigPropertySchema{Type: "boolean", Title: "Unattended", SessionMutable: new(true), ChatMutable: new(true)}
		cfg.Values["unattended"] = value
	}
	// Every setting always has a value.
	cfg.Schema.Required = slices.Sorted(maps.Keys(cfg.Schema.Properties))
	return cfg
}
