package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// fileURI names a workspace-relative or absolute path.
func (p *Project) fileURI(path string) string {
	if !filepath.IsAbs(path) {
		root := "/"
		if p.workspace != nil {
			root = p.workspace.RootPath
		}
		path = filepath.Join(root, path)
	}
	return fileuri.FromPath(path)
}

// message is an input as its client sent it, or as the host knows it.
func (p *Project) message(source json.RawMessage, text string, files, images []string) types.Message {
	var sent types.Message
	if len(source) > 0 && json.Unmarshal(source, &sent) == nil {
		return sent
	}
	message := types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}
	for _, file := range files {
		message.Attachments = append(message.Attachments, types.MessageAttachment{Value: &types.MessageResourceAttachment{Type: "resource", Uri: p.fileURI(file), Label: file}})
	}
	for _, image := range images {
		contentType, data, ok := imageDataURL(image)
		if !ok {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			continue
		}
		message.Attachments = append(message.Attachments, types.MessageAttachment{Value: &types.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: contentType, Label: "Image", Data: data}})
	}
	return message
}

// imageDataURL splits a base64 image data URL into its content type and data.
func imageDataURL(url string) (contentType, data string, ok bool) {
	prefix, data, ok := strings.Cut(url, ",")
	contentType, encoded := strings.CutSuffix(strings.TrimPrefix(prefix, "data:"), ";base64")
	return contentType, data, ok && encoded && strings.HasPrefix(prefix, "data:image/")
}

// decodedMessage is a queued message with the client source it was decoded from.
type decodedMessage struct {
	source  json.RawMessage
	message types.Message
}

// pendingKind is a pending message's kind; one without a kind is queued.
func pendingKind(m types.PendingMessage) types.PendingMessageKind {
	if m.Kind != nil {
		return *m.Kind
	}
	return types.PendingMessageKindQueued
}

func (a *Chat) syncQueue(c *sessionController) {
	next := map[string]types.PendingMessage{}
	order := []string{}
	decoded := map[string]decodedMessage{}
	for _, input := range c.state.PendingInputs {
		kind := types.PendingMessageKindQueued
		if input.Intent == string(code.TurnInputSteer) {
			kind = types.PendingMessageKindSteering
		}
		// A client's source can carry large images, so it is decoded once.
		d, ok := a.decoded[input.ID]
		if !ok || len(input.Source) == 0 || !bytes.Equal(d.source, input.Source) {
			d = decodedMessage{input.Source, a.project.message(input.Source, input.Text, input.Files, input.Images)}
		}
		decoded[input.ID] = d
		next[input.ID] = types.PendingMessage{Id: input.ID, Kind: new(kind), Message: d.message}
		order = append(order, input.ID)
	}
	a.decoded = decoded
	// The message that a starting turn consumes leaves the queue with that turn.
	starting := ""
	if a.chat.ActiveTurn == nil || a.chat.ActiveTurn.Id != c.nextInput {
		starting = c.nextInput
	}
	previous := append([]types.PendingMessage{}, a.chat.QueuedMessages...)
	for _, old := range previous {
		if _, exists := next[old.Id]; !exists && old.Id != starting {
			a.chatAction(types.StateAction{Value: &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: pendingKind(old), Id: old.Id}})
		}
	}
	for _, id := range order {
		item := next[id]
		if !slices.ContainsFunc(previous, func(old types.PendingMessage) bool { return reflect.DeepEqual(old, item) }) {
			a.chatAction(types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: *item.Kind, Id: item.Id, Message: item.Message}})
		}
	}
	shown := []string{}
	for _, message := range a.chat.QueuedMessages {
		if message.Id != starting {
			shown = append(shown, message.Id)
		}
	}
	if !slices.Equal(shown, order) {
		a.chatAction(types.StateAction{Value: &types.ChatQueuedMessagesReorderedAction{Type: types.ActionTypeChatQueuedMessagesReordered, Order: order}})
	}
}

// wantedLocked is the config a chat should run where it runs another: its
// session's values with those a client set on it. Caller holds host.mu.
func (a *Chat) wantedLocked() map[string]json.RawMessage {
	wanted := map[string]json.RawMessage{}
	for key, value := range a.session.state.Config.Values {
		if own, ok := a.own[key]; ok {
			value = own
		}
		if !bytes.Equal(a.config.Values[key], value) {
			wanted[key] = value
		}
	}
	for key, value := range a.own {
		if _, listed := a.session.state.Config.Values[key]; !listed && !bytes.Equal(a.config.Values[key], value) {
			wanted[key] = value
		}
	}
	return wanted
}

// wantedSettings fills a settings command with what its AHP chat wants as it runs.
func (c *sessionController) wantedSettings(command *Command) error {
	c.mu.Lock()
	ch := c.chat
	c.mu.Unlock()
	if ch == nil {
		return nil
	}
	ch.project.host.mu.Lock()
	wanted := ch.wantedLocked()
	ch.project.host.mu.Unlock()
	settings, err := settingsFromConfig(wanted)
	command.Model, command.Effort, command.Mode, command.Unattended = settings.Model, settings.Effort, settings.Mode, settings.Unattended
	return err
}

func (a *Chat) syncConfig(c *sessionController) {
	a.config = configFromSettings(c.state.Settings)
	a.syncOverlay()
}

// syncOverlay shows what a chat sets over its session's values: what a client
// set on it, and where its settings differ from them. Caller holds host.mu.
func (a *Chat) syncOverlay() {
	overlay := map[string]json.RawMessage{}
	for key, value := range a.config.Values {
		_, own := a.own[key]
		if session := a.session.state.Config; own || session == nil || !bytes.Equal(session.Values[key], value) {
			overlay[key] = value
		}
	}
	if a.chat.Config == nil || !reflect.DeepEqual(a.chat.Config.Values, overlay) {
		a.chatAction(types.StateAction{Value: &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: overlay, Replace: new(true)}})
	}
}

// syncUsage reports the active turn's usage: inputTokens is the latest
// request's prompt (the context in use), output and cache reads are this turn's.
func (a *Chat) syncUsage(c *sessionController) {
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	total := c.state.Usage
	usage := types.UsageInfo{InputTokens: new(total.LastInputTokens), OutputTokens: new(total.OutputTokens - a.usageStart.OutputTokens), CacheReadTokens: new(total.CachedTokens - a.usageStart.CachedTokens)}
	if model := c.state.Settings.Model; model != "" {
		usage.Model = &model
	}
	// A provider that counts no tokens still names the model that answers.
	if total.LastInputTokens == 0 && *usage.OutputTokens == 0 {
		if usage = (types.UsageInfo{Model: usage.Model}); usage.Model == nil {
			return
		}
	}
	if active.Usage == nil || !reflect.DeepEqual(*active.Usage, usage) {
		a.chatAction(types.StateAction{Value: &types.ChatUsageAction{Type: types.ActionTypeChatUsage, TurnId: active.Id, Usage: usage}})
	}
}

// syncActivity reports retries, compaction and tool progress as the chat's
// status line.
func (a *Chat) syncActivity(c *sessionController) {
	activity := ""
	switch {
	case c.state.Phase == "retrying" && c.state.Retry != nil:
		activity = fmt.Sprintf("Retrying · %s · attempt %d in %.1fs", c.state.Retry.Reason, c.state.Retry.Attempt, float64(c.state.Retry.DelayMillis)/1000)
	case c.state.Phase == "retrying":
		activity = "Retrying…"
	case c.state.Phase == "compacting":
		activity = "Compacting context…"
	case a.chat.ActiveTurn != nil:
		parts := a.chat.ActiveTurn.ResponseParts
		for i := len(parts) - 1; i >= 0 && activity == ""; i-- {
			if tool, ok := parts[i].Value.(*types.ToolCallResponsePart); ok {
				if running, ok := tool.ToolCall.Value.(*types.ToolCallRunningState); ok && c.state.ToolProgress[running.ToolCallId] != "" {
					activity = running.DisplayName + ": " + c.state.ToolProgress[running.ToolCallId]
				}
			}
		}
	}
	current := ""
	if a.chat.Activity != nil {
		current = *a.chat.Activity
	}
	if activity == current {
		return
	}
	var value *string
	if activity != "" {
		value = &activity
	}
	a.chatAction(types.StateAction{Value: &types.ChatActivityChangedAction{Type: types.ActionTypeChatActivityChanged, Activity: value}})
}

// syncTitle names an untitled session after the first user message; its
// default chat inherits the name.
func (a *Chat) syncTitle(c *sessionController) {
	if a.session.chats[0] != a || a.session.state.Title != "New session" {
		return
	}
	for _, e := range c.entries {
		if e.Type == "user" && e.Content != "" {
			title := []rune(strings.SplitN(e.Content, "\n", 2)[0])
			if len(title) > 80 {
				title = title[:80]
			}
			a.session.sessionAction(types.StateAction{Value: &types.SessionTitleChangedAction{Type: types.ActionTypeSessionTitleChanged, Title: string(title)}})
			return
		}
	}
}
