package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// dispatch applies one client action. Retries carry the same action, so they
// are recognised by the action's own identity: in-progress actions by their
// key, and executed ones by the controller's receipt for a derived command ID.
func (c *connection) dispatch(raw json.RawMessage) {
	h := c.host
	var p types.DispatchActionParams
	if json.Unmarshal(raw, &p) != nil || p.ClientSeq <= 0 || p.Action.Value == nil {
		return
	}
	pending := &pendingAction{params: p, origin: types.ActionOrigin{ClientId: c.id, ClientSeq: p.ClientSeq}}
	key := actionKey(p.Channel, p.Action)
	var a *Session
	var ch *Chat
	var err error
	if h.terminal(p.Channel) != nil {
		err = c.executeTerminalAction(p)
	} else if a, ch, err = h.ensureSession(p.Channel); err == nil {
		h.mu.Lock()
		if ack, ok := a.acks[pending.origin]; ok && ack.key == key {
			c.notify("action", ack.envelope)
			h.mu.Unlock()
			return
		}
		if running := a.pending[key]; running != nil {
			// A retry of an action whose acknowledgement is still due waits for it.
			if running.origin != pending.origin {
				a.remember(pending.origin, key, h.publish(p.Channel, p.Action, &pending.origin, new("action is already in progress")))
			}
			h.mu.Unlock()
			return
		} else {
			a.pending[key] = pending
			h.mu.Unlock()
			err = c.executeAction(a, ch, p)
		}
	}
	// An action for a session or chat that does not exist is ignored, without an echo.
	var rpc *ahp.Error
	if a == nil && errors.As(err, &rpc) && rpc.Code == int(types.ErrorCodeSessionNotFound) {
		return
	}
	var reason *string
	if err != nil {
		reason = new(err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if a == nil { // a terminal's action
		h.publish(p.Channel, p.Action, &pending.origin, reason)
		return
	}
	if a.pending[key] != pending {
		return // the host answered it already
	}
	if err == nil {
		switch p.Action.Value.(type) {
		case *types.ChatTurnStartedAction, *types.ChatTurnCancelledAction:
			// Execution acknowledges these at its actual lifecycle boundary.
			// In particular, cancellation may return before the stream stops.
			return
		}
	}
	a.answer(key, types.StateAction{}, reason)
}

// commandID derives a stable controller command ID. Repeated edits use
// the client identity and sequence; one-shot operations use their entity ID.
func commandID(kind string, values ...any) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(data)
	return "ahp-" + kind + ":" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

// executeAction applies an action to a session (ch is nil) or one of its chats.
func (c *connection) executeAction(a *Session, ch *Chat, p types.DispatchActionParams) error {
	h := c.host
	h.mu.Lock()
	run, err := c.applyAction(a, ch, p)
	h.mu.Unlock()
	defer a.project.saveAliases()
	if err != nil {
		return err
	}
	if run != nil {
		return run()
	}
	return nil
}

// applyAction checks an action and applies what needs no command; it returns
// the rest, which runs without the host's lock. Caller holds host.mu.
func (c *connection) applyAction(a *Session, ch *Chat, p types.DispatchActionParams) (func() error, error) {
	h := c.host
	if h.sessions[a.resource] != a || ch != nil && h.chats[ch.chat.Resource] != ch {
		return nil, ahp.InvalidParams("session disposed")
	}
	chat, session := ch != nil, ch == nil
	if session {
		ch = a.chats[0] // session config and 1.0 clients address the default chat
	}
	command := Command{}
	previous := a.summary()
	switch action := p.Action.Value.(type) {
	case *types.ChatPendingMessageSetAction:
		if !chat || action.Id == "" || action.Message.Origin.Kind != types.MessageKindUser || (action.Kind != types.PendingMessageKindQueued && action.Kind != types.PendingMessageKindSteering) {
			return nil, ahp.InvalidParams("valid user pending message required")
		}
		command.Type, command.ID, command.Text, command.Intent = "send", action.Id, action.Message.Text, code.TurnInputFollowUp
		command.Message, _ = json.Marshal(action.Message)
		if action.Kind == types.PendingMessageKindSteering {
			// Steering waits in the queue for a turn to take it and never starts one.
			command.Intent, command.wait = code.TurnInputSteer, true
		}
		queued := slices.ContainsFunc(ch.chat.QueuedMessages, func(m types.PendingMessage) bool { return m.Id == action.Id }) || ch.chat.SteeringMessage != nil && ch.chat.SteeringMessage.Id == action.Id
		if queued {
			command.Type, command.InputID, command.ID = "queue_update", action.Id, commandID("queue-update", p.Channel, c.id, p.ClientSeq)
		}
		return func() error {
			if err := a.project.attachments(action.Message, &command); err != nil {
				return err
			}
			if err := ch.command(command); err != nil {
				return err
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			// A receipt may outlive its wire acknowledgement. Reply with the
			// current queue state so a retry cannot restore an older edit. An idle
			// queue may also have consumed the message before it was projected.
			if a.pending[actionKey(p.Channel, p.Action)] == nil {
				return nil
			}
			var queued *types.PendingMessage
			for i := range ch.chat.QueuedMessages {
				if ch.chat.QueuedMessages[i].Id == action.Id {
					queued = &ch.chat.QueuedMessages[i]
				}
			}
			if queued == nil && ch.chat.SteeringMessage != nil && ch.chat.SteeringMessage.Id == action.Id {
				queued = ch.chat.SteeringMessage
			}
			if queued == nil {
				ch.chatAction(p.Action)
				ch.chatAction(types.StateAction{Value: &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: action.Kind, Id: action.Id}})
				return nil
			}
			kind := types.PendingMessageKindQueued
			if queued.Kind != nil {
				kind = *queued.Kind
			}
			a.acknowledge(p.Channel, p.Action, types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: kind, Id: queued.Id, Message: queued.Message}})
			return nil
		}, nil
	case *types.ChatPendingMessageRemovedAction:
		if !chat || action.Id == "" {
			return nil, ahp.InvalidParams("chat and pending message required")
		}
		command.Type, command.InputID, command.ID = "queue_remove", action.Id, commandID("queue-remove", action.Id)
		return func() error { return ch.command(command) }, nil
	case *types.ChatQueuedMessagesReorderedAction:
		if !chat {
			return nil, ahp.InvalidParams("chat channel required")
		}
		command.Type, command.Order, command.ID = "queue_reorder", action.Order, commandID("queue-reorder", p.Channel, c.id, p.ClientSeq)
		return func() error { return ch.command(command) }, nil
	case *types.ChatTurnStartedAction:
		if !chat || action.TurnId == "" || ch.chat.ActiveTurn != nil {
			return nil, ahp.InvalidParams("chat must be idle and turnId must be present")
		}
		if slices.ContainsFunc(ch.chat.Turns, func(turn types.Turn) bool { return turn.Id == action.TurnId }) {
			return nil, ahp.InvalidParams("turn already exists")
		}
		return func() error { return ch.start(action.TurnId, action.Message) }, nil
	case *types.ChatTurnCancelledAction:
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId {
			return nil, ahp.InvalidParams("turn is no longer active")
		}
		// Stopping a turn also drops what was queued behind it; a stop that
		// arrives after its turn ended must not reach the next one.
		command.Type, command.ID, command.InputID = "cancel", commandID("cancel", action.TurnId), action.TurnId
		return func() error { return ch.command(command) }, nil
	case *types.ChatTruncatedAction:
		if !chat || action.TurnId != nil && !slices.ContainsFunc(ch.chat.Turns, func(turn types.Turn) bool { return turn.Id == *action.TurnId }) {
			return nil, ahp.InvalidParams("a completed turn of this chat is required")
		}
		command.Type, command.ID = "truncate", commandID("truncate", p.Channel, c.id, p.ClientSeq)
		if action.TurnId != nil {
			command.InputID = *action.TurnId
		}
		return func() error { return ch.command(command) }, nil
	case *types.ChatToolCallConfirmedAction:
		id := strings.TrimPrefix(action.ToolCallId, "approval:")
		prompt := ch.prompts[id]
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindConfirm {
			return nil, ahp.InvalidParams("approval already resolved or expired")
		}
		if action.EditedToolInput != nil {
			return nil, ahp.InvalidParams("editing approval input is not supported")
		}
		answer := "decline"
		if action.Approved {
			answer = "accept"
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", id, answer, commandID("confirm", id, answer, action.SelectedOptionId)
		if id := action.SelectedOptionId; id != nil {
			i := slices.IndexFunc(approvalOptions, func(o types.ConfirmationOption) bool { return o.Id == *id })
			if i < 0 || (approvalOptions[i].Kind == types.ConfirmationOptionKindApprove) != action.Approved {
				return nil, ahp.InvalidParams("unknown approval option")
			}
			command.option = &approvalOptions[i]
		}
		return func() error { return ch.command(command) }, nil
	case *types.ChatInputCompletedAction:
		prompt := ch.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindAsk && prompt.turn != "" {
			return nil, ahp.InvalidParams("input request already resolved or expired")
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", action.RequestId, string(action.Response), commandID("input", action.RequestId, action.Response, action.Answers)
		answers := map[string]types.ChatInputAnswer{}
		maps.Copy(answers, prompt.answers)
		maps.Copy(answers, action.Answers)
		if len(answers) > 0 {
			command.answers = answers
		}
		var err error
		if command.Content, err = promptAnswers(prompt.view, answers); err != nil {
			return nil, err
		}
		return func() error { return ch.command(command) }, nil
	case *types.SessionConfigChangedAction:
		if !session {
			return nil, ahp.InvalidParams("session channel required")
		}
		if _, err := configValues(action.Config); err != nil {
			return nil, err
		}
		return func() error {
			err := a.configure(commandID("config", p.Channel, c.id, p.ClientSeq), action.Config)
			h.mu.Lock()
			defer h.mu.Unlock()
			answer := types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: maps.Clone(a.state.Config.Values), Replace: new(true)}}
			if err != nil {
				a.sessionAction(answer) // what changed before the failure, which the client's action does not acknowledge
			} else {
				sdk.ApplyActionToSession(&a.state, answer)
				a.acknowledge(p.Channel, p.Action, answer)
			}
			for _, ch := range a.chats {
				ch.syncOverlay()
			}
			return err
		}, nil
	case *types.ChatConfigChangedAction:
		if !chat {
			return nil, ahp.InvalidParams("chat channel required")
		}
		values, err := configValues(action.Config)
		if err != nil {
			return nil, err
		}
		// A replaced overlay returns what it omits to the session's values.
		previous, own := ch.own, map[string]json.RawMessage{}
		if action.Replace == nil || !*action.Replace {
			maps.Copy(own, previous)
		}
		maps.Copy(own, values)
		ch.own = own
		return func() error {
			err := ch.syncSettings(commandID("config", p.Channel, c.id, p.ClientSeq))
			h.mu.Lock()
			defer h.mu.Unlock()
			if err != nil {
				if reflect.DeepEqual(ch.own, own) { // a later change stays
					ch.own = previous
				}
				ch.syncOverlay()
				return err
			}
			// The backend may have normalized the values; acknowledge what the chat holds.
			ch.syncOverlay() // a value the chat already had changes no settings
			a.acknowledge(p.Channel, p.Action, types.StateAction{Value: &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: ch.chat.Config.Values, Replace: new(true)}})
			return nil
		}, nil
	case *types.SessionActiveClientSetAction:
		if !session || action.ActiveClient.ClientId != c.id {
			return nil, ahp.InvalidParams("active client identity mismatch")
		}
		a.sessionAction(p.Action)
	case *types.ChatToolCallContentChangedAction:
		call, err := ch.ownedCall(chat, c.id, action.TurnId, action.ToolCallId)
		if err != nil {
			return nil, err
		}
		call.content = append([]types.ToolResultContent(nil), action.Content...) // as the reducer keeps it
		if action.Meta != nil {
			call.meta = action.Meta
		}
		ch.chatAction(p.Action)
	case *types.ChatToolCallCompleteAction:
		call, err := ch.ownedCall(chat, c.id, action.TurnId, action.ToolCallId)
		if err == nil && action.RequiresResultConfirmation != nil && *action.RequiresResultConfirmation {
			err = ahp.InvalidParams("confirming a tool's result is not supported")
		}
		if err != nil {
			return nil, err
		}
		call.complete(action.Result, action.Meta)
		ch.chatAction(p.Action)
	case *types.SessionTitleChangedAction:
		if !session || strings.TrimSpace(action.Title) == "" {
			return nil, ahp.InvalidParams("nonempty session title required")
		}
		a.sessionAction(p.Action)
	case *types.SessionIsReadChangedAction, *types.SessionIsArchivedChangedAction:
		if !session {
			return nil, ahp.InvalidParams("session channel required")
		}
		a.sessionAction(p.Action)
	case *types.ChatDraftChangedAction, *types.ChatIsReadChangedAction, *types.ChatIsArchivedChangedAction:
		if !chat {
			return nil, ahp.InvalidParams("chat channel required")
		}
		ch.chatAction(p.Action)
	case *types.ChatInputAnswerChangedAction:
		prompt := ch.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil {
			return nil, ahp.InvalidParams("input request is no longer active")
		}
		if prompt.answers == nil {
			prompt.answers = map[string]types.ChatInputAnswer{}
		}
		if action.Answer == nil {
			delete(prompt.answers, action.QuestionId)
		} else {
			prompt.answers[action.QuestionId] = *action.Answer
		}
		ch.chatAction(p.Action)
	default:
		return nil, ahp.InvalidParams("action is not supported by this host")
	}
	a.syncChats(previous)
	return nil, nil
}

func answerValues(view PromptView, content map[string]any) map[string]types.ChatInputAnswer {
	var answers map[string]types.ChatInputAnswer
	for _, field := range view.Fields {
		value, ok := content[field.Name]
		if !ok || field.CustomAnswerFor != "" {
			continue
		}
		var answer types.ChatInputAnswerValue
		switch v := value.(type) {
		case string:
			if len(field.Enum) > 0 {
				answer.Value = &types.ChatInputSelectedAnswerValue{Kind: types.ChatInputAnswerValueKindSelected, Value: v}
			} else {
				answer.Value = &types.ChatInputTextAnswerValue{Kind: types.ChatInputAnswerValueKindText, Value: v}
			}
		case bool:
			answer.Value = &types.ChatInputBooleanAnswerValue{Kind: types.ChatInputAnswerValueKindBoolean, Value: v}
		case float64:
			answer.Value = &types.ChatInputNumberAnswerValue{Kind: types.ChatInputAnswerValueKindNumber, Value: v}
		case []string:
			answer.Value = &types.ChatInputSelectedManyAnswerValue{Kind: types.ChatInputAnswerValueKindSelectedMany, Value: v}
		case []any:
			values := []string{}
			for _, item := range v {
				if text, ok := item.(string); ok {
					values = append(values, text)
				}
			}
			answer.Value = &types.ChatInputSelectedManyAnswerValue{Kind: types.ChatInputAnswerValueKindSelectedMany, Value: values}
		default:
			continue
		}
		if answers == nil {
			answers = map[string]types.ChatInputAnswer{}
		}
		answers[field.Name] = types.ChatInputAnswer{Value: &types.ChatInputAnswered{State: types.ChatInputAnswerStateSubmitted, Value: answer}}
	}
	return answers
}

// syncSettings has the chat run its session's values with those a client set on it.
func (a *Chat) syncSettings(id string) error {
	return a.command(Command{ID: id, Type: "settings", sync: true})
}

// settingsFromConfig is the settings command that applies config values.
func settingsFromConfig(values map[string]json.RawMessage) (Command, error) {
	command := Command{Type: "settings"}
	for key, value := range values {
		if key == "unattended" {
			var enabled bool
			if json.Unmarshal(value, &enabled) != nil {
				return Command{}, ahp.InvalidParams("unattended must be a boolean")
			}
			command.Unattended = &enabled
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return Command{}, ahp.InvalidParams("configuration values must be strings")
		}
		switch key {
		case "model":
			command.Model = &text
		case "effort":
			command.Effort = &text
		case "mode":
			command.Mode = &text
		default:
			return Command{}, ahp.InvalidParams("unsupported configuration: " + key)
		}
	}
	return command, nil
}

// configValues checks config values and writes them as the host does, so
// that they compare with the values it projects.
func configValues(values map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if _, err := settingsFromConfig(values); err != nil {
		return nil, err
	}
	written := map[string]json.RawMessage{}
	for key, raw := range values {
		var value any
		_ = json.Unmarshal(raw, &value)
		written[key], _ = json.Marshal(value)
	}
	return written, nil
}

// start runs a client's message as a turn of the idle chat.
func (a *Chat) start(id string, message types.Message) error {
	if message.Agent != nil {
		return ahp.InvalidParams("custom agent selection is not supported")
	}
	command := Command{ID: id, Type: "send", Text: message.Text, requireIdle: true}
	command.Message, _ = json.Marshal(message)
	if err := a.project.attachments(message, &command); err != nil {
		return err
	}
	if message.Model != nil {
		model := message.Model.Id
		if err := a.command(Command{ID: id + ":model", Type: "settings", Model: &model}); err != nil {
			return err
		}
	}
	return a.command(command)
}

func (p *Project) attachments(message types.Message, command *Command) error {
	for _, attachment := range message.Attachments {
		switch a := attachment.Value.(type) {
		case *types.SimpleMessageAttachment:
			if a.ModelRepresentation != nil {
				command.Context = append(command.Context, *a.ModelRepresentation)
			}
		case *types.MessageResourceAttachment:
			path, err := p.resourcePath(a.Uri)
			if err != nil {
				return err
			}
			command.Files = append(command.Files, path)
			if a.Selection != nil {
				data, _ := json.Marshal(a.Selection)
				command.Context = append(command.Context, "Selection in "+path+": "+string(data))
			}
		case *types.MessageEmbeddedResourceAttachment:
			data, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil {
				return ahp.InvalidParams("invalid attachment encoding")
			}
			if strings.HasPrefix(a.ContentType, "image/") {
				command.Images = append(command.Images, "data:"+a.ContentType+";base64,"+a.Data)
			} else if strings.HasPrefix(a.ContentType, "text/") {
				command.Context = append(command.Context, a.Label+":\n"+string(data))
			} else {
				return ahp.InvalidParams("unsupported attachment content type")
			}
		case *types.MessageChatAttachment:
			transcript, err := p.host.chatTranscript(a)
			if err != nil {
				return err
			}
			command.Context = append(command.Context, transcript)
		default:
			return ahp.InvalidParams("unsupported attachment")
		}
	}
	return nil
}

// chatTranscript is a chat as the model of another chat reads it: the turns it
// has now, through the one the attachment names.
func (h *Host) chatTranscript(attachment *types.MessageChatAttachment) (string, error) {
	_, ch, err := h.ensureSession(attachment.Resource)
	if err != nil || ch == nil {
		return "", ahp.InvalidParams("attached chat not found")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	turns := ch.chat.Turns
	if end := attachment.EndTurn; end != nil {
		i := slices.IndexFunc(turns, func(turn types.Turn) bool { return turn.Id == *end })
		if i < 0 {
			return "", ahp.InvalidParams("attached chat has no completed turn " + *end)
		}
		turns = turns[:i+1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<attached_chat label=%q resource=%q>\n", attachment.Label, attachment.Resource)
	for _, turn := range turns {
		// Attachments of its messages stay references.
		b.WriteString("User:\n" + turn.Message.Text + "\n\nAssistant:\n")
		for _, part := range turn.ResponseParts {
			if markdown, ok := part.Value.(*types.MarkdownResponsePart); ok {
				b.WriteString(markdown.Content + "\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("</attached_chat>")
	return b.String(), nil
}

func promptAnswers(view PromptView, answers map[string]types.ChatInputAnswer) (map[string]any, error) {
	content := map[string]any{}
	for key, answer := range answers {
		answered, ok := answer.Value.(*types.ChatInputAnswered)
		if !ok {
			continue
		}
		switch v := answered.Value.Value.(type) {
		case *types.ChatInputTextAnswerValue:
			content[key] = v.Value
		case *types.ChatInputNumberAnswerValue:
			content[key] = v.Value
		case *types.ChatInputBooleanAnswerValue:
			content[key] = v.Value
		case *types.ChatInputSelectedAnswerValue:
			content[key] = v.Value
			if len(v.FreeformValues) > 0 {
				content[key] = strings.Join(v.FreeformValues, "\n")
			}
		case *types.ChatInputSelectedManyAnswerValue:
			content[key] = append(append([]string{}, v.Value...), v.FreeformValues...)
		default:
			return nil, ahp.InvalidParams("unsupported answer type")
		}
	}
	// A free-form answer to an enum field belongs to its companion text field.
	for _, field := range view.Fields {
		if field.CustomAnswerFor == "" {
			continue
		}
		if _, answered := content[field.Name]; answered {
			continue
		}
		parent := slices.IndexFunc(view.Fields, func(f tool.ElicitField) bool { return f.Name == field.CustomAnswerFor })
		if value, ok := content[field.CustomAnswerFor].(string); ok && parent >= 0 && !slices.Contains(view.Fields[parent].Enum, value) {
			content[field.Name] = value
			delete(content, field.CustomAnswerFor)
		}
	}
	return content, nil
}
