package server

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// fileURI names a workspace-relative or absolute path.
func (h *ahpHost) fileURI(path string) string {
	if !filepath.IsAbs(path) {
		root := "/"
		if h.server.workspace != nil {
			root = h.server.workspace.RootPath
		}
		path = filepath.Join(root, path)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func (h *ahpHost) message(text string, files, images []string) t.Message {
	message := t.Message{Text: text, Origin: t.MessageOrigin{Kind: t.MessageKindUser}}
	for _, file := range files {
		message.Attachments = append(message.Attachments, t.MessageAttachment{Value: &t.MessageResourceAttachment{Type: "resource", Uri: h.fileURI(file), Label: file}})
	}
	for _, image := range images {
		prefix, data, ok := strings.Cut(image, ",")
		if !ok || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			continue
		}
		message.Attachments = append(message.Attachments, t.MessageAttachment{Value: &t.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64"), Label: "Image", Data: data}})
	}
	return message
}

func (a *ahpChat) syncQueue(c *sessionController) {
	next := map[string]t.PendingMessage{}
	kinds := map[string]t.PendingMessageKind{}
	for _, input := range c.state.PendingInputs {
		if input.Origin == "task" || input.State != "queued" {
			continue
		}
		kind := t.PendingMessageKindQueued
		if input.Intent == string(code.TurnInputSteer) {
			kind = t.PendingMessageKindSteering
		}
		message := a.host.message(input.Text, input.Files, input.Images)
		next[input.ID] = t.PendingMessage{Id: input.ID, Kind: ahpPtr(kind), Message: message}
		kinds[input.ID] = kind
	}
	previous := append([]t.PendingMessage{}, a.chat.QueuedMessages...)
	for _, old := range previous {
		if _, exists := next[old.Id]; !exists {
			kind := t.PendingMessageKindQueued
			if old.Kind != nil {
				kind = *old.Kind
			}
			a.chatAction(t.StateAction{Value: &t.ChatPendingMessageRemovedAction{Type: t.ActionTypeChatPendingMessageRemoved, Kind: kind, Id: old.Id}})
		}
	}
	for _, input := range c.state.PendingInputs {
		item, exists := next[input.ID]
		if !exists {
			continue
		}
		unchanged := false
		for _, old := range previous {
			if reflect.DeepEqual(old, item) {
				unchanged = true
				break
			}
		}
		if !unchanged {
			a.chatAction(t.StateAction{Value: &t.ChatPendingMessageSetAction{Type: t.ActionTypeChatPendingMessageSet, Kind: kinds[item.Id], Id: item.Id, Message: item.Message}})
		}
	}
}

func (a *ahpChat) syncConfig(c *sessionController) {
	a.config = ahpConfig(c.state.Settings)
	if a.chat.Config == nil || !reflect.DeepEqual(a.chat.Config.Values, a.config.Values) {
		a.chatAction(t.StateAction{Value: &t.ChatConfigChangedAction{Type: t.ActionTypeChatConfigChanged, Config: a.config.Values, Replace: ahpPtr(true)}})
	}
}

// syncUsage reports the active turn's usage: inputTokens is the latest
// request's prompt (the context in use), output and cache reads are this turn's.
func (a *ahpChat) syncUsage(c *sessionController) {
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	total := c.state.Usage
	usage := t.UsageInfo{InputTokens: ahpPtr(total.LastInputTokens), OutputTokens: ahpPtr(total.OutputTokens - a.usageStart.OutputTokens), CacheReadTokens: ahpPtr(total.CachedTokens - a.usageStart.CachedTokens)}
	if model := c.state.Settings.Model; model != "" {
		usage.Model = &model
	}
	if total.LastInputTokens == 0 && *usage.OutputTokens == 0 {
		return
	}
	if active.Usage == nil || !reflect.DeepEqual(*active.Usage, usage) {
		a.chatAction(t.StateAction{Value: &t.ChatUsageAction{Type: t.ActionTypeChatUsage, TurnId: active.Id, Usage: usage}})
	}
}

// syncActivity reports retries, compaction and tool progress as the chat's
// status line.
func (a *ahpChat) syncActivity(c *sessionController) {
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
			if tool, ok := parts[i].Value.(*t.ToolCallResponsePart); ok {
				if running, ok := tool.ToolCall.Value.(*t.ToolCallRunningState); ok && c.state.ToolProgress[running.ToolCallId] != "" {
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
	a.chatAction(t.StateAction{Value: &t.ChatActivityChangedAction{Type: t.ActionTypeChatActivityChanged, Activity: value}})
}

// syncTitle names an untitled default chat, and its session, after the
// first user message.
func (a *ahpChat) syncTitle(c *sessionController) {
	if a.session.chats[0] != a || a.session.state.Title != "New session" {
		return
	}
	for _, e := range c.entries {
		if e.Type == "user" && e.Content != "" {
			title := []rune(strings.SplitN(e.Content, "\n", 2)[0])
			if len(title) > 80 {
				title = title[:80]
			}
			// No chat action renames a chat; its session announces the new summary.
			if a.chat.Title == a.session.state.Title {
				a.chat.Title = string(title)
			}
			a.session.sessionAction(t.StateAction{Value: &t.SessionTitleChangedAction{Type: t.ActionTypeSessionTitleChanged, Title: string(title)}})
			return
		}
	}
}
