package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
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
	key := ahpActionKey(p.Action)
	var a *ahpSession
	var ch *ahpChat
	var err error
	if strings.HasPrefix(p.Channel, "ahp-terminal:") {
		err = c.executeTerminalAction(p)
	} else if a, ch, err = h.ensureSession(p.Channel); err == nil {
		h.mu.Lock()
		if ack, ok := a.acks[pending.origin]; ok && ack.key == key {
			c.notify("action", ack.envelope)
			h.mu.Unlock()
			return
		}
		if running := a.pending[key]; running != nil {
			h.mu.Unlock()
			if running.origin == pending.origin {
				return // a retry of an action whose acknowledgement is still due
			}
			err = ahpInvalid("action is already in progress")
		} else {
			a.pending[key] = pending
			h.mu.Unlock()
			err = c.executeAction(a, ch, p)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if pending.envelope == nil {
		if err == nil {
			switch p.Action.Value.(type) {
			case *t.ChatTurnStartedAction, *t.ChatTurnCancelledAction:
				// Execution acknowledges these at its actual lifecycle boundary.
				// In particular, cancellation may return before the stream stops.
				return
			}
		}
		var reason *string
		if err != nil {
			reason = ahpPtr(err.Error())
		}
		envelope := h.publish(p.Channel, p.Action, &pending.origin, reason)
		pending.envelope = &envelope
		if a != nil {
			a.remember(pending.origin, key, envelope)
		}
	}
	if a != nil && a.pending[key] == pending {
		delete(a.pending, key)
	}
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
	if a.deleted || ch != nil && ch.removed {
		h.mu.Unlock()
		return ahpInvalid("session disposed")
	}
	chat, session := ch != nil, ch == nil
	if session {
		ch = a.chats[0] // session config and 1.0 clients address the default chat
	}
	command := Command{Epoch: ch.controller.epoch}
	previous := a.summary()
	switch action := p.Action.Value.(type) {
	case *t.ChatPendingMessageSetAction:
		if !chat || action.Id == "" || action.Message.Origin.Kind != t.MessageKindUser || (action.Kind != t.PendingMessageKindQueued && action.Kind != t.PendingMessageKindSteering) {
			h.mu.Unlock()
			return ahpInvalid("valid user pending message required")
		}
		command.Type, command.ID, command.Text, command.Intent = "send", action.Id, action.Message.Text, code.TurnInputFollowUp
		if action.Kind == t.PendingMessageKindSteering {
			command.Intent = code.TurnInputSteer
		}
		queued := slices.ContainsFunc(ch.chat.QueuedMessages, func(m t.PendingMessage) bool { return m.Id == action.Id }) || ch.chat.SteeringMessage != nil && ch.chat.SteeringMessage.Id == action.Id
		if queued {
			command.Type, command.InputID, command.ID = "queue_update", action.Id, ahpCommandID("queue-update", p.Channel, c.id, p.ClientSeq)
		}
		h.mu.Unlock()
		if err := h.attachments(action.Message, &command); err != nil {
			return err
		}
		if err := ch.command(command); err != nil {
			return err
		}
		h.mu.Lock()
		// A receipt may outlive its wire acknowledgement. Reply with the
		// current queue state so a retry cannot restore an older edit. An idle
		// queue may also have consumed the message before it was projected.
		if pending := a.pending[ahpActionKey(p.Action)]; pending != nil && pending.envelope == nil {
			var queued *t.PendingMessage
			for i := range ch.chat.QueuedMessages {
				if ch.chat.QueuedMessages[i].Id == action.Id {
					queued = &ch.chat.QueuedMessages[i]
				}
			}
			if queued == nil && ch.chat.SteeringMessage != nil && ch.chat.SteeringMessage.Id == action.Id {
				queued = ch.chat.SteeringMessage
			}
			if queued != nil {
				kind := t.PendingMessageKindQueued
				if queued.Kind != nil {
					kind = *queued.Kind
				}
				a.acknowledge(p.Action, t.StateAction{Value: &t.ChatPendingMessageSetAction{Type: t.ActionTypeChatPendingMessageSet, Kind: kind, Id: queued.Id, Message: queued.Message}})
			} else {
				ch.chatAction(p.Action)
				ch.chatAction(t.StateAction{Value: &t.ChatPendingMessageRemovedAction{Type: t.ActionTypeChatPendingMessageRemoved, Kind: action.Kind, Id: action.Id}})
			}
		}
		h.mu.Unlock()
		return nil
	case *t.ChatPendingMessageRemovedAction:
		if !chat || action.Id == "" {
			h.mu.Unlock()
			return ahpInvalid("chat and pending message required")
		}
		command.Type, command.InputID, command.ID = "queue_remove", action.Id, ahpCommandID("queue-remove", action.Id)
		h.mu.Unlock()
		return ch.command(command)
	case *t.ChatTurnStartedAction:
		if !chat || action.TurnId == "" || ch.chat.ActiveTurn != nil {
			h.mu.Unlock()
			return ahpInvalid("chat must be idle and turnId must be present")
		}
		for _, turn := range ch.chat.Turns {
			if turn.Id == action.TurnId {
				h.mu.Unlock()
				return ahpInvalid("turn already exists")
			}
		}
		h.mu.Unlock()
		command.ID, command.Type, command.Text, command.requireIdle = action.TurnId, "send", action.Message.Text, true
		if err := h.attachments(action.Message, &command); err != nil {
			return err
		}
		if action.Message.Agent != nil {
			return ahpInvalid("custom agent selection is not supported")
		}
		if action.Message.Model != nil {
			model := action.Message.Model.Id
			if err := ch.command(Command{ID: command.ID + ":model", Epoch: command.Epoch, Type: "settings", Model: &model}); err != nil {
				return err
			}
		}
		return ch.command(command)
	case *t.ChatTurnCancelledAction:
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId {
			h.mu.Unlock()
			return ahpInvalid("turn is no longer active")
		}
		// Stopping a turn also drops what was queued behind it; a stop that
		// arrives after its turn ended must not reach the next one.
		command.Type, command.ID, command.InputID, command.ClearQueue = "cancel", ahpCommandID("cancel", action.TurnId), action.TurnId, true
		h.mu.Unlock()
		return ch.command(command)
	case *t.ChatToolCallConfirmedAction:
		id := strings.TrimPrefix(action.ToolCallId, "approval:")
		prompt := ch.prompts[id]
		answer := "decline"
		if action.Approved {
			answer = "accept"
		}
		if !chat || ch.chat.ActiveTurn == nil || ch.chat.ActiveTurn.Id != action.TurnId || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindConfirm {
			h.mu.Unlock()
			return ahpInvalid("approval already resolved or expired")
		}
		if action.EditedToolInput != nil {
			h.mu.Unlock()
			return ahpInvalid("editing approval input is not supported")
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", id, answer, ahpCommandID("confirm", id, answer, action.SelectedOptionId)
		if id := action.SelectedOptionId; id != nil {
			i := slices.IndexFunc(ahpApprovalOptions, func(o t.ConfirmationOption) bool { return o.Id == *id })
			if i < 0 || (ahpApprovalOptions[i].Kind == t.ConfirmationOptionKindApprove) != action.Approved {
				h.mu.Unlock()
				return ahpInvalid("unknown approval option")
			}
			command.option = &ahpApprovalOptions[i]
		}
		h.mu.Unlock()
		return ch.command(command)
	case *t.ChatInputCompletedAction:
		prompt := ch.prompts[action.RequestId]
		id := ahpCommandID("input", action.RequestId, action.Response, action.Answers)
		if !chat || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindAsk && prompt.turn != "" {
			h.mu.Unlock()
			return ahpInvalid("input request already resolved or expired")
		}
		command.Type, command.PromptID, command.Action, command.ID = "prompt_response", action.RequestId, string(action.Response), id
		answers := maps.Clone(prompt.answers)
		if answers == nil {
			answers = map[string]t.ChatInputAnswer{}
		}
		maps.Copy(answers, action.Answers)
		var err error
		command.Content, err = ahpAnswers(prompt.view, answers)
		h.mu.Unlock()
		if err != nil {
			return err
		}
		return ch.command(command)
	case *t.SessionConfigChangedAction:
		if !session {
			h.mu.Unlock()
			return ahpInvalid("session channel required")
		}
		h.mu.Unlock()
		if err := ch.setConfig(ahpCommandID("config", p.Channel, c.id, p.ClientSeq), action.Config); err != nil {
			return err
		}
		// The backend may have normalized the values; acknowledge what it holds.
		h.mu.Lock()
		a.acknowledge(p.Action, t.StateAction{Value: &t.SessionConfigChangedAction{Type: t.ActionTypeSessionConfigChanged, Config: a.state.Config.Values, Replace: ahpPtr(true)}})
		h.mu.Unlock()
		return nil
	case *t.ChatConfigChangedAction:
		if !chat {
			h.mu.Unlock()
			return ahpInvalid("chat channel required")
		}
		h.mu.Unlock()
		if err := ch.setConfig(ahpCommandID("config", p.Channel, c.id, p.ClientSeq), action.Config); err != nil {
			return err
		}
		h.mu.Lock()
		a.acknowledge(p.Action, t.StateAction{Value: &t.ChatConfigChangedAction{Type: t.ActionTypeChatConfigChanged, Config: ch.chat.Config.Values, Replace: ahpPtr(true)}})
		h.mu.Unlock()
		return nil
	case *t.SessionActiveClientSetAction:
		if !session || action.ActiveClient.ClientId != c.id {
			h.mu.Unlock()
			return ahpInvalid("active client identity mismatch")
		}
		// Tools from attached clients are deliberately not advertised to agents.
		a.sessionAction(p.Action)
	case *t.SessionTitleChangedAction:
		if !session || strings.TrimSpace(action.Title) == "" {
			h.mu.Unlock()
			return ahpInvalid("nonempty session title required")
		}
		a.sessionAction(p.Action)
	case *t.SessionIsReadChangedAction, *t.SessionIsArchivedChangedAction:
		if !session {
			h.mu.Unlock()
			return ahpInvalid("session channel required")
		}
		a.sessionAction(p.Action)
	case *t.ChatDraftChangedAction, *t.ChatIsReadChangedAction, *t.ChatIsArchivedChangedAction:
		if !chat {
			h.mu.Unlock()
			return ahpInvalid("chat channel required")
		}
		ch.chatAction(p.Action)
	case *t.ChatInputAnswerChangedAction:
		prompt := ch.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil {
			h.mu.Unlock()
			return ahpInvalid("input request is no longer active")
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
		h.mu.Unlock()
		return ahpInvalid("action is not supported by this host")
	}
	a.syncChats(previous)
	h.mu.Unlock()
	return nil
}

func ahpAnswerValues(view PromptView, content map[string]any) map[string]t.ChatInputAnswer {
	answers := map[string]t.ChatInputAnswer{}
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
		answers[field.Name] = t.ChatInputAnswer{Value: &t.ChatInputAnswered{State: t.ChatInputAnswerStateSubmitted, Value: answer}}
	}
	return answers
}

func (a *ahpChat) setConfig(id string, values map[string]json.RawMessage) error {
	command := Command{ID: id, Epoch: a.controller.epoch, Type: "settings"}
	for key, value := range values {
		if key == "unattended" {
			var enabled bool
			if json.Unmarshal(value, &enabled) != nil {
				return ahpInvalid("unattended must be a boolean")
			}
			command.Unattended = &enabled
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return ahpInvalid("configuration values must be strings")
		}
		switch key {
		case "model":
			command.Model = &text
		case "effort":
			command.Effort = &text
		case "mode":
			command.Mode = &text
		default:
			return ahpInvalid("unsupported configuration: " + key)
		}
	}
	return a.command(command)
}

func (h *ahpHost) attachments(message t.Message, command *Command) error {
	for _, attachment := range message.Attachments {
		switch a := attachment.Value.(type) {
		case *t.SimpleMessageAttachment:
			if a.ModelRepresentation != nil {
				command.Context = append(command.Context, *a.ModelRepresentation)
			}
		case *t.MessageResourceAttachment:
			path, err := h.resourcePath(a.Uri)
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
		default:
			return ahpInvalid("unsupported attachment")
		}
	}
	return nil
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
