package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"github.com/adrianliechti/wingman-agent/pkg/code"

	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func (c *ahpConnection) dispatch(raw json.RawMessage) {
	h := c.host
	var p t.DispatchActionParams
	if json.Unmarshal(raw, &p) != nil || p.ClientSeq <= 0 || p.Action.Value == nil {
		return
	}
	data, _ := json.Marshal(p)
	fingerprint := sha256.Sum256(data)
	h.mu.Lock()
	ledger := h.receipts[c.id]
	if ledger == nil {
		ledger = &ahpClientReceipts{entries: map[int64]*ahpActionReceipt{}}
		h.receipts[c.id] = ledger
	}
	h.mu.Unlock()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	h.mu.Lock()
	if previous, ok := ledger.entries[p.ClientSeq]; ok {
		if previous.fingerprint == fingerprint {
			if previous.envelope != nil {
				c.notify("action", previous.envelope)
			}
		} else {
			reason := "clientSeq reused with different content"
			h.publish(p.Channel, p.Action, &t.ActionOrigin{ClientId: c.id, ClientSeq: p.ClientSeq}, &reason)
		}
		h.mu.Unlock()
		return
	}
	if p.ClientSeq <= ledger.last {
		reason := "clientSeq is older than the retained receipt window"
		h.publish(p.Channel, p.Action, &t.ActionOrigin{ClientId: c.id, ClientSeq: p.ClientSeq}, &reason)
		h.mu.Unlock()
		return
	}
	receipt := &ahpActionReceipt{fingerprint: fingerprint}
	ledger.last = p.ClientSeq
	ledger.entries[p.ClientSeq] = receipt
	h.mu.Unlock()
	var a *ahpSession
	var err error
	pending := &ahpPendingAction{params: p, origin: t.ActionOrigin{ClientId: c.id, ClientSeq: p.ClientSeq}, receipt: receipt}
	if strings.HasPrefix(p.Channel, "ahp-terminal:") {
		err = c.executeTerminalAction(p)
	} else {
		a, err = h.ensureSession(p.Channel)
	}
	if err == nil && a != nil {
		h.mu.Lock()
		if a.pending[ahpActionKey(p.Action)] != nil {
			err = ahpInvalid("action is already in progress")
		} else {
			a.pending[ahpActionKey(p.Action)] = pending
		}
		h.mu.Unlock()
		if err == nil {
			err = c.executeAction(a, p)
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
		receipt.envelope = &envelope
	}
	if a != nil && a.pending[ahpActionKey(p.Action)] == pending {
		delete(a.pending, ahpActionKey(p.Action))
	}
	// Retain a bounded retry window; older sequences are rejected, never rerun.
	if len(ledger.entries) > 256 {
		oldest := p.ClientSeq
		for seq, entry := range ledger.entries {
			if seq < oldest && entry.envelope != nil {
				oldest = seq
			}
		}
		delete(ledger.entries, oldest)
	}
}

func (c *ahpConnection) executeAction(a *ahpSession, p t.DispatchActionParams) error {
	h := c.host
	command := Command{ID: fmt.Sprintf("ahp:%s:%d", c.id, p.ClientSeq), Epoch: a.controller.epoch}
	h.mu.Lock()
	if a.deleted {
		h.mu.Unlock()
		return ahpInvalid("session disposed")
	}
	chat := p.Channel == a.chat.Resource
	session := p.Channel == a.resource
	if !chat && !session {
		h.mu.Unlock()
		return ahpInvalid("unknown channel")
	}
	switch action := p.Action.Value.(type) {
	case *t.ChatPendingMessageSetAction:
		if !chat || action.Id == "" || action.Message.Origin.Kind != t.MessageKindUser || (action.Kind != t.PendingMessageKindQueued && action.Kind != t.PendingMessageKindSteering) {
			h.mu.Unlock(); return ahpInvalid("valid user pending message required")
		}
		command.Type, command.ID, command.Text, command.Intent = "send", action.Id, action.Message.Text, code.TurnInputFollowUp
		if action.Kind == t.PendingMessageKindSteering { command.Intent = code.TurnInputSteer }
		for _, queued := range a.chat.QueuedMessages { if queued.Id == action.Id { command.Type, command.InputID, command.ID = "queue_update", action.Id, fmt.Sprintf("ahp:%s:%d", c.id, p.ClientSeq) } }
		if a.chat.SteeringMessage != nil && a.chat.SteeringMessage.Id == action.Id { command.Type, command.InputID, command.ID = "queue_update", action.Id, fmt.Sprintf("ahp:%s:%d", c.id, p.ClientSeq) }
		h.mu.Unlock()
		if err := h.attachments(action.Message, &command); err != nil { return err }
		return a.command(command)
	case *t.ChatPendingMessageRemovedAction:
		if !chat || action.Id == "" { h.mu.Unlock(); return ahpInvalid("chat and pending message required") }
		command.Type, command.InputID = "queue_remove", action.Id
		h.mu.Unlock()
		return a.command(command)
	case *t.ChatTurnStartedAction:
		if !chat || action.TurnId == "" || a.chat.ActiveTurn != nil {
			h.mu.Unlock()
			return ahpInvalid("chat must be idle and turnId must be present")
		}
		for _, turn := range a.chat.Turns {
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
			if err := a.command(Command{ID: command.ID + ":model", Epoch: command.Epoch, Type: "settings", Model: &model}); err != nil {
				return err
			}
		}
		return a.command(command)
	case *t.ChatTurnCancelledAction:
		if !chat || a.chat.ActiveTurn == nil || a.chat.ActiveTurn.Id != action.TurnId {
			h.mu.Unlock()
			return ahpInvalid("turn is no longer active")
		}
		command.Type = "cancel"
		h.mu.Unlock()
		return a.command(command)
	case *t.ChatToolCallConfirmedAction:
		id := strings.TrimPrefix(action.ToolCallId, "approval:")
		prompt := a.prompts[id]
		if !chat || a.chat.ActiveTurn == nil || a.chat.ActiveTurn.Id != action.TurnId || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindConfirm {
			h.mu.Unlock()
			return ahpInvalid("approval already resolved or expired")
		}
		if action.EditedToolInput != nil {
			h.mu.Unlock()
			return ahpInvalid("editing approval input is not supported")
		}
		command.Type, command.PromptID, command.Action = "prompt_response", id, "decline"
		if action.Approved {
			command.Action = "accept"
		}
		h.mu.Unlock()
		return a.command(command)
	case *t.ChatInputCompletedAction:
		prompt := a.prompts[action.RequestId]
		if !chat || prompt == nil || prompt.response != nil || prompt.view.Kind != PromptKindAsk {
			h.mu.Unlock()
			return ahpInvalid("input request already resolved or expired")
		}
		command.Type, command.PromptID, command.Action = "prompt_response", action.RequestId, string(action.Response)
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
		return a.command(command)
	case *t.SessionConfigChangedAction:
		if !session {
			h.mu.Unlock()
			return ahpInvalid("session channel required")
		}
		h.mu.Unlock()
		return a.setConfig(command.ID, action.Config)
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
		a.chatAction(p.Action)
	case *t.ChatInputAnswerChangedAction:
		prompt := a.prompts[action.RequestId]
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
		a.chatAction(p.Action)
	default:
		h.mu.Unlock()
		return ahpInvalid("action is not supported by this host")
	}
	a.syncInputNeeded()
	summary := a.summary()
	h.notifyRoot("root/sessionSummaryChanged", t.SessionSummaryChangedParams{Channel: t.RootResourceURI, Session: a.resource, Changes: t.PartialSessionSummary{Title: &summary.Title, Status: &summary.Status}})
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

func (a *ahpSession) setConfig(id string, values map[string]json.RawMessage) error {
	command := Command{ID: id, Epoch: a.controller.epoch, Type: "settings"}
	for key, value := range values {
		if key == "unattended" {
			var enabled bool
			if json.Unmarshal(value, &enabled) != nil { return ahpInvalid("unattended must be a boolean") }
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
				command.Text += "\n\n" + *a.ModelRepresentation
			}
		case *t.MessageResourceAttachment:
			path, err := h.resourcePath(a.Uri)
			if err != nil {
				return err
			}
			command.Files = append(command.Files, path)
			if a.Selection != nil {
				data, _ := json.Marshal(a.Selection)
				command.Text += "\nSelection in " + path + ": " + string(data)
			}
		case *t.MessageEmbeddedResourceAttachment:
			data, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil {
				return ahpInvalid("invalid attachment encoding")
			}
			if strings.HasPrefix(a.ContentType, "image/") {
				command.Images = append(command.Images, "data:"+a.ContentType+";base64,"+a.Data)
			} else if strings.HasPrefix(a.ContentType, "text/") {
				command.Text += "\n\n" + a.Label + ":\n" + string(data)
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
	for _, field := range view.Fields {
		if field.CustomAnswerFor != "" {
			if value, ok := content[field.CustomAnswerFor].(string); ok {
				content[field.Name] = value
			}
		}
	}
	return content, nil
}
