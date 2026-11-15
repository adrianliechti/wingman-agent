package server

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Chat projects one native session (its controller) as an AHP chat.
type Chat struct {
	project          *Project
	session          *Session
	controller       *sessionController
	chat             types.ChatState
	config           *types.SessionConfigState  // the controller's, read without its lock
	own              map[string]json.RawMessage // the config a client set on this chat, which it keeps over the session's
	prompts          map[string]*prompt
	promptSeq        int
	reasoning        map[string]*reasoningTiming
	endings          map[string]types.StateAction // the action that ended each turn the host ended
	usageStart       UsageView                    // the controller's totals when the active turn started
	offered          map[string]string            // the client behind each tool the agent was last given
	clientCalls      map[string]*clientCall
	decoded          map[string]decodedMessage // the queued messages by input
	projected        bool                      // turns created after the first projection are live, not history
	projectedInput   string
	projectedOutcome string
	projectedPrompts []PromptView
}

// The host is the producer of reasoning timing: a run starts at its first
// text and ends when the next content part starts or the turn ends.
type reasoningTiming struct {
	started   time.Time
	startedAt string
	duration  *int64
}

type prompt struct {
	view     PromptView
	turn     string // empty when no turn was active
	after    string // the entry it follows in its turn
	seq      int
	response *Command
	answers  map[string]types.ChatInputAnswer
}

// Approvals are answered once; the agent asks again for later operations.
var approvalOptions = []types.ConfirmationOption{
	{Id: "once", Label: "Allow", Kind: types.ConfirmationOptionKindApprove},
	{Id: "deny", Label: "Deny", Kind: types.ConfirmationOptionKindDeny},
}

// A retried dispatch carries its original origin; it is answered with the
// original acknowledgement instead of being applied again.
type ack struct {
	key      string
	envelope types.ActionEnvelope
}

const ackLimit = 512

func (a *Session) remember(origin types.ActionOrigin, key string, envelope types.ActionEnvelope) {
	if _, exists := a.acks[origin]; !exists {
		a.ackOrder = append(a.ackOrder, origin)
		if len(a.ackOrder) > ackLimit {
			delete(a.acks, a.ackOrder[0])
			a.ackOrder = a.ackOrder[1:]
		}
	}
	a.acks[origin] = ack{key: key, envelope: envelope}
}

// pendingAction is a client's action that the host has not answered yet.
type pendingAction struct {
	params types.DispatchActionParams
	origin types.ActionOrigin
}

func newChat(a *Session, c *sessionController, resource, title, stamp string) *Chat {
	config := configFromSettings(c.state.Settings)
	return &Chat{project: a.project, session: a, controller: c, config: config, prompts: map[string]*prompt{}, reasoning: map[string]*reasoningTiming{}, clientCalls: map[string]*clientCall{}, endings: map[string]types.StateAction{}, own: map[string]json.RawMessage{},
		chat: types.ChatState{Resource: resource, Title: title, Status: types.SessionStatusIdle, ModifiedAt: stamp, Turns: []types.Turn{}, Config: &types.ChatConfigState{Values: map[string]json.RawMessage{}}},
	}
}

// emit publishes an action, acknowledging the client action it completes.
func (a *Session) emit(channel string, action types.StateAction) {
	if len(a.pending) == 0 || !a.answer(actionKey(channel, action), action, nil) {
		a.project.host.publish(channel, action, nil, nil)
	}
}

// acknowledge answers a client's action with the host's own, such as the
// values a backend normalized.
func (a *Session) acknowledge(channel string, client, action types.StateAction) {
	a.answer(actionKey(channel, client), action, nil)
}

// answer publishes the host's answer to a pending client action: the action
// that completes it, or, without one, the client's own, rejected when a
// reason is given. It reports whether the action was pending. Caller holds host.mu.
func (a *Session) answer(key string, action types.StateAction, rejection *string) bool {
	p := a.pending[key]
	if p == nil {
		return false
	}
	delete(a.pending, key)
	if action.Value == nil {
		action = p.params.Action
	}
	a.remember(p.origin, key, a.project.host.publish(p.params.Channel, action, &p.origin, rejection))
	return true
}

// actionKey identifies an action on a channel: the host's action that
// completes a client's has the client's key.
func actionKey(channel string, action types.StateAction) string {
	switch v := action.Value.(type) {
	case *types.ChatPendingMessageSetAction:
		return channel + " pending-set:" + v.Id
	case *types.ChatPendingMessageRemovedAction:
		return channel + " pending-remove:" + v.Id
	case *types.ChatTurnStartedAction:
		return channel + " start:" + v.TurnId
	case *types.ChatTurnCancelledAction:
		return channel + " cancel:" + v.TurnId
	case *types.ChatToolCallConfirmedAction:
		return channel + " confirm:" + v.ToolCallId
	case *types.ChatInputCompletedAction:
		return channel + " input:" + v.RequestId
	default:
		data, _ := json.Marshal(action)
		return channel + " " + string(data)
	}
}

func (a *Chat) chatAction(action types.StateAction) {
	ahp.ApplyActionToChat(&a.chat, action)
	a.session.emit(a.chat.Resource, action)
}

func (a *Chat) syncLocked(c *sessionController, changes []SessionChange) {
	a.project.host.mu.Lock()
	defer func() {
		a.project.host.mu.Unlock()
		a.project.saveAliases()
	}()
	if !a.project.availableLocked() || a.project.host.chats[a.chat.Resource] != a {
		return
	}
	if c.state.Status == sessionDeleted {
		a.session.chatDeleted(a)
		return
	}
	// Token deltas do not rebuild or copy the completed conversation.
	if len(changes) == 1 && changes[0].Type == "entry.append" && a.chat.ActiveTurn != nil {
		change := changes[0]
		for _, part := range a.chat.ActiveTurn.ResponseParts {
			switch p := part.Value.(type) {
			case *types.MarkdownResponsePart:
				if p.Id == change.ID {
					a.chatAction(types.StateAction{Value: &types.ChatDeltaAction{Type: types.ActionTypeChatDelta, TurnId: a.chat.ActiveTurn.Id, PartId: p.Id, Content: change.Text}})
					return
				}
			case *types.ReasoningResponsePart:
				if p.Id == change.ID {
					a.chatAction(types.StateAction{Value: &types.ChatReasoningAction{Type: types.ActionTypeChatReasoning, TurnId: a.chat.ActiveTurn.Id, PartId: p.Id, Content: change.Text, StartedAt: a.startReasoning(p.Id, change.Text)}})
					return
				}
			}
		}
	}
	// Updates of a running tool change only its own part.
	if len(changes) == 1 && changes[0].Type == "entry.upsert" && changes[0].Entry != nil && changes[0].Entry.Type == "tool" && a.chat.ActiveTurn != nil {
		part, _ := a.entryPart(*changes[0].Entry)
		next := part.Value.(*types.ToolCallResponsePart).ToolCall
		for _, p := range a.chat.ActiveTurn.ResponseParts {
			if tool, ok := p.Value.(*types.ToolCallResponsePart); ok && toolCallID(tool.ToolCall) == toolCallID(next) {
				if reflect.DeepEqual(tool.ToolCall, next) || a.syncTool(a.chat.ActiveTurn.Id, tool.ToolCall, next) {
					return
				}
				break
			}
		}
	}
	a.syncProjection(c, changes)
}

func toolCallID(call types.ToolCallState) string {
	switch c := call.Value.(type) {
	case *types.ToolCallStreamingState:
		return c.ToolCallId
	case *types.ToolCallRunningState:
		return c.ToolCallId
	case *types.ToolCallPendingConfirmationState:
		return c.ToolCallId
	case *types.ToolCallCompletedState:
		return c.ToolCallId
	case *types.ToolCallCancelledState:
		return c.ToolCallId
	}
	return ""
}

func (a *Chat) syncProjection(c *sessionController, changes []SessionChange) {
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
				a.prompts[p.ID] = &prompt{view: p, turn: turn, after: after, seq: a.promptSeq}
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
			state := types.TurnStateComplete
			if len(turns) > 0 {
				state = turns[len(turns)-1].State
			}
			// A prompt a completed turn left open was not its own; it stays answerable as the session's.
			for _, p := range a.prompts {
				if state == types.TurnStateComplete && p.turn == old.Id && p.response == nil {
					p.turn = ""
				}
			}
			a.finishTurn(old.Id, state, c.state.Error)
			// The finished turn now carries its duration.
			turns, active = a.transcript(c)
		}
		changed := !reflect.DeepEqual(a.chat.Turns, turns)
		// A client's cut leaves the turns the agent kept.
		if active == nil && changed {
			for _, p := range a.session.pending {
				if _, cut := p.params.Action.Value.(*types.ChatTruncatedAction); cut && p.params.Channel == a.chat.Resource {
					a.chatAction(p.params.Action)
					changed = !reflect.DeepEqual(a.chat.Turns, turns)
					break
				}
			}
		}
		if changed {
			a.chatAction(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated}})
			if active == nil && len(turns) > 0 && turns[len(turns)-1].State == types.TurnStateError {
				last := turns[len(turns)-1]
				a.chatAction(types.StateAction{Value: &types.ChatTurnsLoadedAction{Type: types.ActionTypeChatTurnsLoaded, Turns: turns[:len(turns)-1]}})
				started := time.Now().UTC().Format(time.RFC3339Nano)
				if last.StartedAt != nil {
					started = *last.StartedAt
				}
				a.chatAction(types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: last.Id, StartedAt: started, Message: last.Message}})
				for _, part := range last.ResponseParts {
					if _, isError := part.Value.(*types.ErrorResponsePart); !isError {
						a.chatAction(types.StateAction{Value: &types.ChatResponsePartAction{Type: types.ActionTypeChatResponsePart, TurnId: last.Id, Part: part}})
					}
				}
				a.finishTurn(last.Id, types.TurnStateError, c.state.Error)
			} else {
				a.chatAction(types.StateAction{Value: &types.ChatTurnsLoadedAction{Type: types.ActionTypeChatTurnsLoaded, Turns: turns}})
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
				a.chatAction(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: keep}})
				a.chatAction(types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: active.Id, StartedAt: active.StartedAt, Message: active.Message}})
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
func (a *Chat) startTurn(active *types.ActiveTurn) {
	started := &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: active.Id, StartedAt: active.StartedAt, Message: active.Message}
	// An idle chat consumes a message before its queue ever held it.
	if p := a.session.pending[actionKey(a.chat.Resource, types.StateAction{Value: &types.ChatPendingMessageSetAction{Id: active.Id}})]; p != nil {
		a.chatAction(p.params.Action)
	}
	if i := slices.IndexFunc(a.chat.QueuedMessages, func(m types.PendingMessage) bool { return m.Id == active.Id }); i >= 0 {
		a.chatAction(types.StateAction{Value: &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: pendingKind(a.chat.QueuedMessages[i]), Id: active.Id}})
		started.QueuedMessageId = &active.Id
	}
	a.chatAction(types.StateAction{Value: started})
}

func (a *Chat) finishTurn(id string, state types.TurnState, message *string) {
	var duration int64
	if active := a.chat.ActiveTurn; active != nil && active.Id == id {
		if started, err := time.Parse(time.RFC3339Nano, active.StartedAt); err == nil {
			duration = max(0, time.Since(started).Milliseconds())
		}
	}
	ending := types.StateAction{Value: &types.ChatTurnCompleteAction{Type: types.ActionTypeChatTurnComplete, TurnId: id, Duration: duration}}
	switch state {
	case types.TurnStateCancelled:
		ending = types.StateAction{Value: &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: id, Duration: duration}}
	case types.TurnStateError:
		text := "Turn failed"
		if message != nil {
			text = *message
		}
		ending = types.StateAction{Value: &types.ChatErrorAction{Type: types.ActionTypeChatError, TurnId: id, Duration: duration, Part: types.ErrorResponsePart{Kind: types.ResponsePartKindError, Error: types.ErrorInfo{ErrorType: "agent", Message: text}}}}
	}
	a.endings[id] = ending
	a.chatAction(ending)
	if state == types.TurnStateCancelled {
		return
	}
	// A cancel that lost the race is answered once its client can see the turn ended.
	key := actionKey(a.chat.Resource, types.StateAction{Value: &types.ChatTurnCancelledAction{TurnId: id}})
	a.session.answer(key, types.StateAction{}, new("the turn finished before it could be cancelled"))
}

func (a *Chat) transcript(c *sessionController) ([]types.Turn, *types.ActiveTurn) {
	old := append([]types.Turn{}, a.chat.Turns...)
	if active := a.chat.ActiveTurn; active != nil {
		old = append(old, types.Turn{Id: active.Id, StartedAt: &active.StartedAt, Message: active.Message, ResponseParts: active.ResponseParts, Usage: active.Usage, State: types.TurnStateComplete})
	}
	turns := []types.Turn{}
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
				last.ResponseParts = append(last.ResponseParts, types.ResponsePart{Value: &types.SystemNotificationResponsePart{Kind: types.ResponsePartKindSystemNotification, Content: types.NewStringOrMarkdownPlain(e.Content)}})
				anchors[len(anchors)-1] = append(anchors[len(anchors)-1], e.ID)
				continue
			}
			id := e.InputID
			if id == "" {
				id = e.ID
			}
			var startedAt, duration = (*string)(nil), (*int64)(nil)
			var usage *types.UsageInfo
			var message *types.Message
			if len(turns) < len(old) {
				prior := old[len(turns)]
				// External backends may omit InputID in their committed history.
				// Preserve turn identity at the controller's ordered history boundary.
				if e.InputID == "" {
					id = prior.Id
				}
				if prior.Id == id {
					startedAt, duration, usage = prior.StartedAt, prior.Duration, prior.Usage
					message = &prior.Message
					message.Text = e.Content
				}
			} else if a.projected {
				startedAt = new(time.Now().UTC().Format(time.RFC3339Nano))
			}
			// Only a turn the chat does not show yet has its message built.
			if message == nil {
				message = new(a.project.message(e.Source, e.Content, e.Files, e.Images))
				if e.Source == nil && e.Content == "" && len(e.Files)+len(e.Images) == 0 {
					message.Origin.Kind = types.MessageKindSystemNotification // such as a background task's result
				}
			}
			turns = append(turns, types.Turn{Id: id, StartedAt: startedAt, Duration: duration, Usage: usage, Message: *message, ResponseParts: []types.ResponsePart{}, State: types.TurnStateComplete})
			anchors = append(anchors, []string{e.ID})
			continue
		}
		if len(turns) == 0 {
			continue
		}
		if part, ok := a.entryPart(e); ok {
			if p, ok := part.Value.(*types.ReasoningResponsePart); ok {
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
		prompts := []*prompt{}
		for _, p := range a.prompts {
			if p.turn == turn.Id {
				prompts = append(prompts, p)
			}
		}
		slices.SortFunc(prompts, func(x, y *prompt) int { return x.seq - y.seq })
		// A prompt stays after the entry it followed, so later parts append to the turn.
		for _, p := range prompts {
			at := len(anchors[i]) - 1
			for at >= 0 && anchors[i][at] != p.after {
				at--
			}
			if at < 0 {
				at = len(anchors[i]) - 1
			}
			turn.ResponseParts = slices.Insert(turn.ResponseParts, at, promptPart(p))
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
		return a.ended(turns[:len(turns)-1]), &types.ActiveTurn{Id: last.Id, StartedAt: started, Message: last.Message, ResponseParts: last.ResponseParts}
	}
	switch code.TurnInputState(c.lastOutcome) {
	case code.TurnInputCancelled:
		last.State = types.TurnStateCancelled
	case code.TurnInputFailed:
		last.State = types.TurnStateError
	}
	// A failed turn restored from history ends with the session's error.
	if _, ended := a.endings[last.Id]; !ended && last.State == types.TurnStateError {
		message := "Turn failed"
		if c.state.Error != nil {
			message = *c.state.Error
		}
		last.ResponseParts = append(last.ResponseParts, types.ResponsePart{Value: &types.ErrorResponsePart{Kind: types.ResponsePartKindError, Error: types.ErrorInfo{ErrorType: "agent", Message: message}}})
	}
	return a.ended(turns), nil
}

// ended finishes the turns the host ended the way the reducer finished them for its clients.
func (a *Chat) ended(turns []types.Turn) []types.Turn {
	ids := map[string]bool{}
	for i, turn := range turns {
		ids[turn.Id] = true
		ending, ok := a.endings[turn.Id]
		if !ok {
			continue
		}
		active := &types.ActiveTurn{Id: turn.Id, Message: turn.Message, ResponseParts: turn.ResponseParts, Usage: turn.Usage}
		if turn.StartedAt != nil {
			active.StartedAt = *turn.StartedAt
		}
		scratch := types.ChatState{ActiveTurn: active, Turns: []types.Turn{}}
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

func (p *Project) entryPart(e TranscriptEntry) (types.ResponsePart, bool) {
	switch e.Type {
	case "assistant":
		var content strings.Builder
		content.WriteString(e.Content)
		for _, image := range e.Images {
			fmt.Fprintf(&content, "\n\n![Image](%s)", image)
		}
		return types.ResponsePart{Value: &types.MarkdownResponsePart{Kind: types.ResponsePartKindMarkdown, Id: e.ID, Content: content.String()}}, true
	case "reasoning":
		return types.ResponsePart{Value: &types.ReasoningResponsePart{Kind: types.ResponsePartKindReasoning, Id: e.ID, Content: e.Content}}, true
	case "notice":
		return types.ResponsePart{Value: &types.SystemNotificationResponsePart{Kind: types.ResponsePartKindSystemNotification, Content: types.NewStringOrMarkdownPlain(e.Content)}}, true
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
		var content []types.ToolResultContent
		if e.ToolResult != nil {
			content = append(content, types.ToolResultContent{Value: &types.ToolResultTextContent{Type: types.ToolResultContentTypeText, Text: *e.ToolResult}})
		}
		for _, location := range e.ToolLocations {
			uri := p.fileURI(location.Path)
			if location.Line > 0 {
				uri += "#L" + strconv.Itoa(location.Line)
			}
			content = append(content, types.ToolResultContent{Value: &types.ToolResultResourceContent{Type: types.ToolResultContentTypeResource, Uri: uri}})
		}
		for _, image := range e.Images {
			if contentType, data, ok := imageDataURL(image); ok {
				content = append(content, types.ToolResultContent{Value: &types.ToolResultEmbeddedResourceContent{Type: types.ToolResultContentTypeEmbeddedResource, ContentType: contentType, Data: data}})
			}
		}
		invocation := types.NewStringOrMarkdownPlain(e.ToolName)
		input := &types.ToolInput{Inline: &e.ToolArgs}
		var call types.ToolCallState
		if e.ToolResult != nil {
			call.Value = &types.ToolCallCompletedState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: types.ToolCallStatusCompleted, InvocationMessage: invocation, ToolInput: input, Confirmed: types.ToolCallConfirmationReasonNotNeeded, Success: true, PastTenseMessage: invocation, Content: content}
		} else if e.ToolPartial {
			call.Value = &types.ToolCallStreamingState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: types.ToolCallStatusStreaming, PartialInput: &e.ToolArgs}
		} else {
			call.Value = &types.ToolCallRunningState{Meta: meta, ToolCallId: id, ToolName: e.ToolName, DisplayName: e.ToolName, Intention: intention, Status: types.ToolCallStatusRunning, InvocationMessage: invocation, ToolInput: input, Confirmed: types.ToolCallConfirmationReasonNotNeeded, Content: content}
		}
		return types.ResponsePart{Value: &types.ToolCallResponsePart{Kind: types.ResponsePartKindToolCall, ToolCall: call}}, true
	}
	return types.ResponsePart{}, false
}

func (a *Chat) syncParts(parts []types.ResponsePart) bool {
	active := a.chat.ActiveTurn
	if len(active.ResponseParts) > len(parts) {
		return false
	}
	for i, next := range parts {
		if i >= len(active.ResponseParts) {
			switch p := next.Value.(type) {
			case *types.MarkdownResponsePart, *types.ToolCallResponsePart:
				a.closeReasoning()
			case *types.ReasoningResponsePart:
				a.closeReasoning()
				if stamp := a.startReasoning(p.Id, p.Content); stamp != nil {
					p.StartedAt = stamp
				}
			}
			if call, ok := next.Value.(*types.ToolCallResponsePart); ok {
				a.appendTool(active.Id, call.ToolCall)
			} else if input, ok := next.Value.(*types.InputRequestResponsePart); ok && input.Response == nil {
				a.chatAction(types.StateAction{Value: &types.ChatInputRequestedAction{Type: types.ActionTypeChatInputRequested, Request: input.Request}})
			} else {
				a.chatAction(types.StateAction{Value: &types.ChatResponsePartAction{Type: types.ActionTypeChatResponsePart, TurnId: active.Id, Part: next}})
			}
			continue
		}
		previous := active.ResponseParts[i]
		if reflect.DeepEqual(previous, next) {
			continue
		}
		switch n := next.Value.(type) {
		case *types.MarkdownResponsePart:
			p, ok := previous.Value.(*types.MarkdownResponsePart)
			if !ok || p.Id != n.Id || !strings.HasPrefix(n.Content, p.Content) {
				return false
			}
			a.chatAction(types.StateAction{Value: &types.ChatDeltaAction{Type: types.ActionTypeChatDelta, TurnId: active.Id, PartId: n.Id, Content: strings.TrimPrefix(n.Content, p.Content)}})
		case *types.ReasoningResponsePart:
			p, ok := previous.Value.(*types.ReasoningResponsePart)
			if !ok || p.Id != n.Id || !strings.HasPrefix(n.Content, p.Content) {
				return false
			}
			a.chatAction(types.StateAction{Value: &types.ChatReasoningAction{Type: types.ActionTypeChatReasoning, TurnId: active.Id, PartId: n.Id, Content: strings.TrimPrefix(n.Content, p.Content), StartedAt: a.startReasoning(n.Id, n.Content)}})
		case *types.ToolCallResponsePart:
			p, ok := previous.Value.(*types.ToolCallResponsePart)
			if !ok {
				return false
			}
			if !a.syncTool(active.Id, p.ToolCall, n.ToolCall) {
				return false
			}
		case *types.InputRequestResponsePart:
			p, ok := previous.Value.(*types.InputRequestResponsePart)
			if !ok || p.Request.Id != n.Request.Id || n.Response == nil {
				return false
			}
			a.chatAction(types.StateAction{Value: &types.ChatInputCompletedAction{Type: types.ActionTypeChatInputCompleted, RequestId: n.Request.Id, Response: *n.Response, Answers: n.Request.Answers}})
		default:
			return false
		}
	}
	return true
}

func (a *Chat) startReasoning(id, content string) *string {
	if content == "" || a.reasoning[id] != nil {
		return nil
	}
	now := time.Now()
	timing := &reasoningTiming{started: now, startedAt: now.UTC().Format(time.RFC3339Nano)}
	a.reasoning[id] = timing
	return &timing.startedAt
}

func (a *Chat) closeReasoning() {
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	for _, part := range active.ResponseParts {
		p, ok := part.Value.(*types.ReasoningResponsePart)
		if !ok {
			continue
		}
		timing := a.reasoning[p.Id]
		if timing == nil || timing.duration != nil {
			continue
		}
		timing.duration = new(max(0, time.Since(timing.started).Milliseconds()))
		a.chatAction(types.StateAction{Value: &types.ChatReasoningAction{Type: types.ActionTypeChatReasoning, TurnId: active.Id, PartId: p.Id, Duration: timing.duration}})
	}
}

func (a *Chat) appendTool(turn string, call types.ToolCallState) {
	var id, name, display string
	var intention *string
	var meta map[string]json.RawMessage
	var contributor *types.ToolCallContributor
	switch c := call.Value.(type) {
	case *types.ToolCallStreamingState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *types.ToolCallRunningState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *types.ToolCallPendingConfirmationState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	case *types.ToolCallCompletedState:
		id, name, display, intention = c.ToolCallId, c.ToolName, c.DisplayName, c.Intention
		meta, contributor = c.Meta, c.Contributor
	default:
		a.chatAction(types.StateAction{Value: &types.ChatResponsePartAction{Type: types.ActionTypeChatResponsePart, TurnId: turn, Part: types.ResponsePart{Value: &types.ToolCallResponsePart{Kind: types.ResponsePartKindToolCall, ToolCall: call}}}})
		return
	}
	a.chatAction(types.StateAction{Value: &types.ChatToolCallStartAction{Type: types.ActionTypeChatToolCallStart, TurnId: turn, ToolCallId: id, ToolName: name, DisplayName: display, Intention: intention, Meta: meta, Contributor: contributor}})
	switch c := call.Value.(type) {
	case *types.ToolCallPendingConfirmationState:
		a.chatAction(types.StateAction{Value: &types.ChatToolCallReadyAction{Type: types.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: id, InvocationMessage: c.InvocationMessage, ToolInput: c.ToolInput, Options: c.Options}})
	case *types.ToolCallStreamingState:
		if c.PartialInput != nil {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallDeltaAction{Type: types.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: id, Content: c.PartialInput}})
		}
	default:
		a.syncTool(turn, types.ToolCallState{Value: &types.ToolCallStreamingState{ToolCallId: id}}, call)
	}
}

func (a *Chat) syncTool(turn string, previous, next types.ToolCallState) bool {
	switch n := next.Value.(type) {
	case *types.ToolCallStreamingState:
		p, ok := previous.Value.(*types.ToolCallStreamingState)
		if !ok || p.ToolCallId != n.ToolCallId || p.PartialInput == nil || n.PartialInput == nil || !strings.HasPrefix(*n.PartialInput, *p.PartialInput) {
			return false
		}
		a.chatAction(types.StateAction{Value: &types.ChatToolCallDeltaAction{Type: types.ActionTypeChatToolCallDelta, TurnId: turn, ToolCallId: n.ToolCallId, Content: new(strings.TrimPrefix(*n.PartialInput, *p.PartialInput))}})
	case *types.ToolCallRunningState:
		var content []types.ToolResultContent
		if p, ok := previous.Value.(*types.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed, SelectedOptionId: optionID(n.SelectedOption)}})
		} else if p, ok := previous.Value.(*types.ToolCallStreamingState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallReadyAction{Type: types.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: n.ToolCallId, InvocationMessage: n.InvocationMessage, ToolInput: n.ToolInput, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*types.ToolCallRunningState); ok && p.ToolCallId == n.ToolCallId {
			content = p.Content
		} else {
			return false
		}
		if !reflect.DeepEqual(content, n.Content) {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallContentChangedAction{Type: types.ActionTypeChatToolCallContentChanged, TurnId: turn, ToolCallId: n.ToolCallId, Content: n.Content}})
		}
	case *types.ToolCallCompletedState:
		if p, ok := previous.Value.(*types.ToolCallPendingConfirmationState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: true, Confirmed: &n.Confirmed, SelectedOptionId: optionID(n.SelectedOption)}})
		} else if p, ok := previous.Value.(*types.ToolCallStreamingState); ok && p.ToolCallId == n.ToolCallId {
			a.chatAction(types.StateAction{Value: &types.ChatToolCallReadyAction{Type: types.ActionTypeChatToolCallReady, TurnId: turn, ToolCallId: n.ToolCallId, InvocationMessage: n.InvocationMessage, ToolInput: n.ToolInput, Confirmed: &n.Confirmed}})
		} else if p, ok := previous.Value.(*types.ToolCallRunningState); !ok || p.ToolCallId != n.ToolCallId {
			return false
		}
		a.chatAction(types.StateAction{Value: &types.ChatToolCallCompleteAction{Type: types.ActionTypeChatToolCallComplete, TurnId: turn, ToolCallId: n.ToolCallId, Result: types.ToolCallResult{Success: n.Success, PastTenseMessage: n.PastTenseMessage, Content: n.Content, StructuredContent: n.StructuredContent, ResultMetadata: n.ResultMetadata, Error: n.Error}}})
	case *types.ToolCallCancelledState:
		p, ok := previous.Value.(*types.ToolCallPendingConfirmationState)
		if !ok || p.ToolCallId != n.ToolCallId {
			return false
		}
		a.chatAction(types.StateAction{Value: &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: turn, ToolCallId: n.ToolCallId, Approved: false, Reason: &n.Reason, SelectedOptionId: optionID(n.SelectedOption)}})
	default:
		return false
	}
	return true
}

func optionID(option *types.ConfirmationOption) *string {
	if option == nil {
		return nil
	}
	return &option.Id
}

func (a *Chat) resolvePromptLocked(command Command) {
	a.project.host.mu.Lock()
	defer a.project.host.mu.Unlock()
	if p := a.prompts[command.PromptID]; p != nil {
		p.response = &command
		switch {
		case command.answers != nil:
			p.answers = command.answers
		case len(command.Content) > 0: // answered by a client of another protocol
			p.answers = answerValues(p.view, command.Content)
		}
		if len(p.answers) == 0 {
			p.answers = nil
		}
	}
}

func promptPart(p *prompt) types.ResponsePart {
	if p.view.Kind == PromptKindConfirm {
		id := "approval:" + p.view.ID
		message := types.NewStringOrMarkdownPlain(p.view.Message)
		var call types.ToolCallState
		if p.response == nil {
			call.Value = &types.ToolCallPendingConfirmationState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: types.ToolCallStatusPendingConfirmation, InvocationMessage: message, Options: approvalOptions}
		} else if p.response.Action == "accept" {
			call.Value = &types.ToolCallCompletedState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: types.ToolCallStatusCompleted, InvocationMessage: message, Confirmed: types.ToolCallConfirmationReasonUserAction, SelectedOption: p.response.option, Success: true, PastTenseMessage: types.NewStringOrMarkdownPlain("Approved")}
		} else {
			call.Value = &types.ToolCallCancelledState{ToolCallId: id, ToolName: "approval", DisplayName: "Approval", Status: types.ToolCallStatusCancelled, InvocationMessage: message, Reason: types.ToolCallCancellationReasonDenied, SelectedOption: p.response.option}
		}
		return types.ResponsePart{Value: &types.ToolCallResponsePart{Kind: types.ResponsePartKindToolCall, ToolCall: call}}
	}
	request := types.ChatInputRequest{Id: p.view.ID, Message: &p.view.Message, Questions: []types.ChatInputQuestion{}, Answers: p.answers}
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
		var q types.ChatInputQuestion
		if len(f.Enum) > 0 {
			options := []types.ChatInputOption{}
			for i, v := range f.Enum {
				o := types.ChatInputOption{Id: v, Label: v}
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
				q.Value = &types.ChatInputMultiSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: types.ChatInputQuestionKindMultiSelect, Options: options, AllowFreeformInput: freeform}
			} else {
				q.Value = &types.ChatInputSingleSelectQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: types.ChatInputQuestionKindSingleSelect, Options: options, AllowFreeformInput: freeform}
			}
		} else if f.Type == "boolean" {
			question := &types.ChatInputBooleanQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: types.ChatInputQuestionKindBoolean}
			if value, ok := f.Default.(bool); ok {
				question.DefaultValue = &value
			}
			q.Value = question
		} else if f.Type == "number" || f.Type == "integer" {
			question := &types.ChatInputNumberQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: types.ChatInputQuestionKindNumber}
			if value, ok := f.Default.(float64); ok {
				question.DefaultValue = &value
			}
			q.Value = question
		} else {
			question := &types.ChatInputTextQuestion{Id: f.Name, Title: &f.Title, Message: message, Required: &f.Required, Kind: types.ChatInputQuestionKindText}
			if value, ok := f.Default.(string); ok {
				question.DefaultValue = &value
			}
			q.Value = question
		}
		request.Questions = append(request.Questions, q)
	}
	part := &types.InputRequestResponsePart{Kind: types.ResponsePartKindInputRequest, Request: request}
	if p.response != nil {
		response := types.ChatInputResponseKind(p.response.Action)
		part.Response = &response
	}
	return types.ResponsePart{Value: part}
}

// promptRequest is a prompt as an input request; an approval has no questions.
func promptRequest(p *prompt) types.ChatInputRequest {
	if p.view.Kind == PromptKindConfirm {
		return types.ChatInputRequest{Id: p.view.ID, Message: &p.view.Message, Questions: []types.ChatInputQuestion{}}
	}
	return promptPart(p).Value.(*types.InputRequestResponsePart).Request
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

func configFromSettings(settings SessionSettings) *types.SessionConfigState {
	cfg := &types.SessionConfigState{Schema: types.SessionConfigSchema{Type: "object", Properties: map[string]types.SessionConfigPropertySchema{}}, Values: map[string]json.RawMessage{}}
	add := func(key, title, current string, values, labels []string) {
		if len(values) == 0 {
			return
		}
		// A default that followed the value would change the schema without an action.
		property := types.SessionConfigPropertySchema{Type: "string", Title: title, SessionMutable: new(true), ChatMutable: new(true), EnumLabels: labels}
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
		cfg.Schema.Properties["unattended"] = types.SessionConfigPropertySchema{Type: "boolean", Title: "Unattended", SessionMutable: new(true), ChatMutable: new(true)}
		cfg.Values["unattended"] = value
	}
	// Every setting always has a value.
	cfg.Schema.Required = slices.Sorted(maps.Keys(cfg.Schema.Properties))
	return cfg
}
