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
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// dispatch applies one client action. Retries carry the same action, so they
// are recognised by the action's own identity: in-progress actions by their
// key, and executed ones by the controller's receipt for a derived command ID.
func (c *ahpConnection) dispatch(raw json.RawMessage) {
	h := c.host
	var p t.DispatchActionParams
	if json.Unmarshal(raw, &p) != nil || p.ClientSeq <= 0 || p.Action.Value == nil {
		return
	}
	pending := &ahpPendingAction{params: p, origin: t.ActionOrigin{ClientId: c.id, ClientSeq: p.ClientSeq}}
	key := ahpActionKey(p.Channel, p.Action)
	var a *ahpSession
	var ch *ahpChat
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
	var rpc *ahpRPCError
	if a == nil && errors.As(err, &rpc) && rpc.Code == int(t.ErrorCodeSessionNotFound) {
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
		case *t.ChatTurnStartedAction, *t.ChatTurnCancelledAction:
			// Execution acknowledges these at its actual lifecycle boundary.
			// In particular, cancellation may return before the stream stops.
			return
		}
	}
	a.answer(key, t.StateAction{}, reason)
}

// ahpCommandID derives a stable controller command ID. Repeated edits use
// the client identity and sequence; one-shot operations use their entity ID.
func ahpCommandID(kind string, values ...any) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(data)
	return "ahp-" + kind + ":" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

// executeAction applies an action to a session (ch is nil) or one of its chats.
func (c *ahpConnection) executeAction(a *ahpSession, ch *ahpChat, p t.DispatchActionParams) error {
	h := c.host
	h.mu.Lock()
	run, err := c.applyAction(a, ch, p)
	unsaved := a.unsaved
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if run != nil {
		return run()
	}
	if unsaved {
		a.save()
	}
	return nil
}

// applyAction checks an action and applies what needs no command; it returns
// the rest, which runs without the host's lock. Caller holds host.mu.
func (c *ahpConnection) applyAction(a *ahpSession, ch *ahpChat, p t.DispatchActionParams) (func() error, error) {
	h := c.host
	if a.deleted || ch != nil && ch.removed {
		return nil, ahpInvalid("session disposed")
	}
	chat, session := ch != nil, ch == nil
	if session {
		ch = a.chats[0] // session config and 1.0 clients address the default chat
	}
	command := Command{}
	previous := a.summary()
	switch action := p.Action.Value.(type) {
	case *t.ChatPendingMessageSetAction:
		if !chat || action.Id == "" || action.Message.Origin.Kind != t.MessageKindUser || (action.Kind != t.PendingMessageKindQueued && action.Kind != t.PendingMessageKindSteering) {
			return nil, ahpInvalid("valid user pending message required")
		}
		command.Type, command.ID, command.Text, command.Intent = "send", action.Id, action.Message.Text, code.TurnInputFollowUp
		command.Message, _ = json.Marshal(action.Message)
		if action.Kind == t.PendingMessageKindSteering {
			// Steering waits in the queue for a turn to take it and never starts one.
			command.Intent, command.wait = code.TurnInputSteer, true
		}
		queued := slices.ContainsFunc(ch.chat.QueuedMessages, func(m t.PendingMessage) bool { return m.Id == action.Id }) || ch.chat.SteeringMessage != nil && ch.chat.SteeringMessage.Id == action.Id
		if queued {
			command.Type, command.InputID, command.ID = "queue_update", action.Id, ahpCommandID("queue-update", p.Channel, c.id, p.ClientSeq)
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
			if a.pending[ahpActionKey(p.Channel, p.Action)] == nil {
				return nil
			}
			var queued *t.PendingMessage
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
				ch.chatAction(t.StateAction{Value: &t.ChatPendingMessageRemovedAction{Type: t.ActionTypeChatPendingMessageRemoved, Kind: action.Kind, Id: action.Id}})
				return nil
			}
			kind := t.PendingMessageKindQueued
			if queued.Kind != nil {
				kind = *queued.Kind
			}
			a.acknowledge(p.Channel, p.Action, t.StateAction{Value: &t.ChatPendingMessageSetAction{Type: t.ActionTypeChatPendingMessageSet, Kind: kind, Id: queued.Id, Message: queued.Message}})
			return nil
		}, nil
	case *t.ChatPendingMessageRemovedAction:
		if !chat || action.Id == "" {
			return nil, ahpInvalid("chat and pending message required")
		}
		command.Type, command.InputID, command.ID = "queue_remove", action.Id, ahpCommandID("queue-remove", action.Id)
		return func() error { return ch.command(command) }, nil
	case *t.ChatQueuedMessagesReorderedAction:
		if !chat {
			return nil, ahpInvalid("chat channel required")
		}
		command.Type, command.Order, command.ID = "queue_reorder", action.Order, ahpCommandID("queue-reorder", p.Channel, c.id, p.ClientSeq)
		return func() error { return ch.command(command) }, nil
	case *t.ChatTurnStartedAction:
		if !chat || action.TurnId == "" || ch.chat.ActiveTurn != nil {
			return nil, ahpInvalid("chat must be idle and turnId must be present")
		}
		if slices.ContainsFunc(ch.chat.Turns, func(turn t.Turn) bool { return turn.Id == action.TurnId }) {
			return nil, ahpInvalid("turn already exists")
		}
		return func() error { return ch.start(action.TurnId, action.Message) }, nil
	case *t.ChatTurnCancelledAction:
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId {
			return nil, ahpInvalid("turn is no longer active")
		}
		// Stopping a turn also drops what was queued behind it; a stop that
		// arrives after its turn ended must not reach the next one.
		command.Type, command.ID, command.InputID = "cancel", ahpCommandID("cancel", action.TurnId), action.TurnId
		return func() error { return ch.command(command) }, nil
	case *t.ChatTruncatedAction:
		if !chat || action.TurnId != nil && !slices.ContainsFunc(ch.chat.Turns, func(turn t.Turn) bool { return turn.Id == *action.TurnId }) {
			return nil, ahpInvalid("a completed turn of this chat is required")
		}
		command.Type, command.ID = "truncate", ahpCommandID("truncate", p.Channel, c.id, p.ClientSeq)
		if action.TurnId != nil {
			command.InputID = *action.TurnId
		}
		return func() error { return ch.command(command) }, nil
	case *t.ChatToolCallConfirmedAction:
		id := strings.TrimPrefix(action.ToolCallId, "approval:")
		prompt := ch.prompts[id]
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindConfirm {
			return nil, ahpInvalid("approval already resolved or expired")
		}
		if action.EditedToolInput != nil {
			return nil, ahpInvalid("editing approval input is not supported")
		}
		answer := "decline"
		if action.Approved {
			answer = "accept"
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", id, answer, ahpCommandID("confirm", id, answer, action.SelectedOptionId)
		if id := action.SelectedOptionId; id != nil {
			i := slices.IndexFunc(ahpApprovalOptions, func(o t.ConfirmationOption) bool { return o.Id == *id })
			if i < 0 || (ahpApprovalOptions[i].Kind == t.ConfirmationOptionKindApprove) != action.Approved {
				return nil, ahpInvalid("unknown approval option")
			}
			command.option = &ahpApprovalOptions[i]
		}
		return func() error { return ch.command(command) }, nil
	case *t.ChatInputCompletedAction:
		prompt := ch.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindAsk && prompt.turn != "" {
			return nil, ahpInvalid("input request already resolved or expired")
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", action.RequestId, string(action.Response), ahpCommandID("input", action.RequestId, action.Response, action.Answers)
		answers := map[string]t.ChatInputAnswer{}
		maps.Copy(answers, prompt.answers)
		maps.Copy(answers, action.Answers)
		if len(answers) > 0 {
			command.answers = answers
		}
		var err error
		if command.Content, err = ahpAnswers(prompt.view, answers); err != nil {
			return nil, err
		}
		return func() error { return ch.command(command) }, nil
	case *t.SessionConfigChangedAction:
		if !session {
			return nil, ahpInvalid("session channel required")
		}
		if _, err := ahpConfigValues(action.Config); err != nil {
			return nil, err
		}
		return func() error {
			err := a.configure(ahpCommandID("config", p.Channel, c.id, p.ClientSeq), action.Config)
			h.mu.Lock()
			defer h.mu.Unlock()
			answer := t.StateAction{Value: &t.SessionConfigChangedAction{Type: t.ActionTypeSessionConfigChanged, Config: maps.Clone(a.state.Config.Values), Replace: new(true)}}
			if err != nil {
				a.sessionAction(answer) // what changed before the failure, which the client's action does not acknowledge
			} else {
				ahp.ApplyActionToSession(&a.state, answer)
				a.acknowledge(p.Channel, p.Action, answer)
			}
			for _, ch := range a.chats {
				ch.syncOverlay()
			}
			return err
		}, nil
	case *t.ChatConfigChangedAction:
		if !chat {
			return nil, ahpInvalid("chat channel required")
		}
		values, err := ahpConfigValues(action.Config)
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
			err := ch.syncSettings(ahpCommandID("config", p.Channel, c.id, p.ClientSeq))
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
			a.acknowledge(p.Channel, p.Action, t.StateAction{Value: &t.ChatConfigChangedAction{Type: t.ActionTypeChatConfigChanged, Config: ch.chat.Config.Values, Replace: new(true)}})
			return nil
		}, nil
	case *t.SessionActiveClientSetAction:
		if !session || action.ActiveClient.ClientId != c.id {
			return nil, ahpInvalid("active client identity mismatch")
		}
		a.sessionAction(p.Action)
	case *t.ChatToolCallContentChangedAction:
		call, err := ch.ownedCall(chat, c.id, action.TurnId, action.ToolCallId)
		if err != nil {
			return nil, err
		}
		call.content = append([]t.ToolResultContent(nil), action.Content...) // as the reducer keeps it
		if action.Meta != nil {
			call.meta = action.Meta
		}
		ch.chatAction(p.Action)
	case *t.ChatToolCallCompleteAction:
		call, err := ch.ownedCall(chat, c.id, action.TurnId, action.ToolCallId)
		if err == nil && action.RequiresResultConfirmation != nil && *action.RequiresResultConfirmation {
			err = ahpInvalid("confirming a tool's result is not supported")
		}
		if err != nil {
			return nil, err
		}
		call.complete(action.Result, action.Meta)
		ch.chatAction(p.Action)
	case *t.SessionTitleChangedAction:
		if !session || strings.TrimSpace(action.Title) == "" {
			return nil, ahpInvalid("nonempty session title required")
		}
		a.sessionAction(p.Action)
	case *t.SessionIsReadChangedAction, *t.SessionIsArchivedChangedAction:
		if !session {
			return nil, ahpInvalid("session channel required")
		}
		a.sessionAction(p.Action)
	case *t.ChatDraftChangedAction, *t.ChatIsReadChangedAction, *t.ChatIsArchivedChangedAction:
		if !chat {
			return nil, ahpInvalid("chat channel required")
		}
		ch.chatAction(p.Action)
	case *t.ChatInputAnswerChangedAction:
		prompt := ch.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil {
			return nil, ahpInvalid("input request is no longer active")
		}
		if prompt.answers == nil {
			prompt.answers = map[string]t.ChatInputAnswer{}
		}
		if action.Answer == nil {
			delete(prompt.answers, action.QuestionId)
		} else {
			prompt.answers[action.QuestionId] = *action.Answer
		}
		ch.chatAction(p.Action)
	default:
		return nil, ahpInvalid("action is not supported by this host")
	}
	a.syncChats(previous)
	return nil, nil
}

func ahpAnswerValues(view PromptView, content map[string]any) map[string]t.ChatInputAnswer {
	var answers map[string]t.ChatInputAnswer
	for _, field := range view.Fields {
		value, ok := content[field.Name]
		if !ok || field.CustomAnswerFor != "" {
			continue
		}
		var answer t.ChatInputAnswerValue
		switch v := value.(type) {
		case string:
			if len(field.Enum) > 0 {
				answer.Value = &t.ChatInputSelectedAnswerValue{Kind: t.ChatInputAnswerValueKindSelected, Value: v}
			} else {
				answer.Value = &t.ChatInputTextAnswerValue{Kind: t.ChatInputAnswerValueKindText, Value: v}
			}
		case bool:
			answer.Value = &t.ChatInputBooleanAnswerValue{Kind: t.ChatInputAnswerValueKindBoolean, Value: v}
		case float64:
			answer.Value = &t.ChatInputNumberAnswerValue{Kind: t.ChatInputAnswerValueKindNumber, Value: v}
		case []string:
			answer.Value = &t.ChatInputSelectedManyAnswerValue{Kind: t.ChatInputAnswerValueKindSelectedMany, Value: v}
		case []any:
			values := []string{}
			for _, item := range v {
				if text, ok := item.(string); ok {
					values = append(values, text)
				}
			}
			answer.Value = &t.ChatInputSelectedManyAnswerValue{Kind: t.ChatInputAnswerValueKindSelectedMany, Value: values}
		default:
			continue
		}
		if answers == nil {
			answers = map[string]t.ChatInputAnswer{}
		}
		answers[field.Name] = t.ChatInputAnswer{Value: &t.ChatInputAnswered{State: t.ChatInputAnswerStateSubmitted, Value: answer}}
	}
	return answers
}

// syncSettings has the chat run its session's values with those a client set on it.
func (a *ahpChat) syncSettings(id string) error {
	return a.command(Command{ID: id, Type: "settings", sync: true})
}

// ahpSettings is the settings command that applies config values.
func ahpSettings(values map[string]json.RawMessage) (Command, error) {
	command := Command{Type: "settings"}
	for key, value := range values {
		if key == "unattended" {
			var enabled bool
			if json.Unmarshal(value, &enabled) != nil {
				return Command{}, ahpInvalid("unattended must be a boolean")
			}
			command.Unattended = &enabled
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return Command{}, ahpInvalid("configuration values must be strings")
		}
		switch key {
		case "model":
			command.Model = &text
		case "effort":
			command.Effort = &text
		case "mode":
			command.Mode = &text
		default:
			return Command{}, ahpInvalid("unsupported configuration: " + key)
		}
	}
	return command, nil
}

// ahpConfigValues checks config values and writes them as the host does, so
// that they compare with the values it projects.
func ahpConfigValues(values map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if _, err := ahpSettings(values); err != nil {
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
func (a *ahpChat) start(id string, message t.Message) error {
	if message.Agent != nil {
		return ahpInvalid("custom agent selection is not supported")
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

func (p *ahpProject) attachments(message t.Message, command *Command) error {
	for _, attachment := range message.Attachments {
		switch a := attachment.Value.(type) {
		case *t.SimpleMessageAttachment:
			if a.ModelRepresentation != nil {
				command.Context = append(command.Context, *a.ModelRepresentation)
			}
		case *t.MessageResourceAttachment:
			path, err := p.resourcePath(a.Uri)
			if err != nil {
				return err
			}
			command.Files = append(command.Files, path)
			if a.Selection != nil {
				data, _ := json.Marshal(a.Selection)
				command.Context = append(command.Context, "Selection in "+path+": "+string(data))
			}
		case *t.MessageEmbeddedResourceAttachment:
			data, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil {
				return ahpInvalid("invalid attachment encoding")
			}
			if strings.HasPrefix(a.ContentType, "image/") {
				command.Images = append(command.Images, "data:"+a.ContentType+";base64,"+a.Data)
			} else if strings.HasPrefix(a.ContentType, "text/") {
				command.Context = append(command.Context, a.Label+":\n"+string(data))
			} else {
				return ahpInvalid("unsupported attachment content type")
			}
		case *t.MessageChatAttachment:
			transcript, err := p.chatTranscript(a)
			if err != nil {
				return err
			}
			command.Context = append(command.Context, transcript)
		default:
			return ahpInvalid("unsupported attachment")
		}
	}
	return nil
}

// chatTranscript is a chat as the model of another chat reads it: the turns it
// has now, through the one the attachment names.
func (h *ahpHost) chatTranscript(attachment *t.MessageChatAttachment) (string, error) {
	_, ch, err := h.ensureSession(attachment.Resource)
	if err != nil || ch == nil {
		return "", ahpInvalid("attached chat not found")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	turns := ch.chat.Turns
	if end := attachment.EndTurn; end != nil {
		i := slices.IndexFunc(turns, func(turn t.Turn) bool { return turn.Id == *end })
		if i < 0 {
			return "", ahpInvalid("attached chat has no completed turn " + *end)
		}
		turns = turns[:i+1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<attached_chat label=%q resource=%q>\n", attachment.Label, attachment.Resource)
	for _, turn := range turns {
		// Attachments of its messages stay references.
		b.WriteString("User:\n" + turn.Message.Text + "\n\nAssistant:\n")
		for _, part := range turn.ResponseParts {
			if markdown, ok := part.Value.(*t.MarkdownResponsePart); ok {
				b.WriteString(markdown.Content + "\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("</attached_chat>")
	return b.String(), nil
}

func ahpAnswers(view PromptView, answers map[string]t.ChatInputAnswer) (map[string]any, error) {
	content := map[string]any{}
	for key, answer := range answers {
		answered, ok := answer.Value.(*t.ChatInputAnswered)
		if !ok {
			continue
		}
		switch v := answered.Value.Value.(type) {
		case *t.ChatInputTextAnswerValue:
			content[key] = v.Value
		case *t.ChatInputNumberAnswerValue:
			content[key] = v.Value
		case *t.ChatInputBooleanAnswerValue:
			content[key] = v.Value
		case *t.ChatInputSelectedAnswerValue:
			content[key] = v.Value
			if len(v.FreeformValues) > 0 {
				content[key] = strings.Join(v.FreeformValues, "\n")
			}
		case *t.ChatInputSelectedManyAnswerValue:
			content[key] = append(append([]string{}, v.Value...), v.FreeformValues...)
		default:
			return nil, ahpInvalid("unsupported answer type")
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
