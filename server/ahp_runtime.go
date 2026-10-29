package server

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Wingman-specific presentation data complements AHP's transcript, prompts and
// queue. Execution remains owned by the session controller and TurnManager.
type ahpRuntimeInfo struct {
	Ref          SessionRef        `json:"ref"`
	Epoch        string            `json:"epoch"`
	Status       string            `json:"status"`
	Phase        string            `json:"phase"`
	Retry        any               `json:"retry,omitempty"`
	Error        *string           `json:"error"`
	Compaction   *CompactionView   `json:"compaction,omitempty"`
	Usage        UsageView         `json:"usage"`
	QueuePaused  bool              `json:"queuePaused"`
	CanSteer     bool              `json:"canSteer"`
	Settings     SessionSettings   `json:"settings"`
	ToolProgress map[string]string `json:"toolProgress"`
}

func (a *ahpSession) syncRuntime(c *sessionController) {
	state := c.state
	data, _ := json.Marshal(ahpRuntimeInfo{Ref: c.ref, Epoch: c.epoch, Status: state.Status, Phase: state.Phase, Retry: state.Retry, Error: state.Error, Compaction: state.Compaction, Usage: state.Usage, QueuePaused: state.QueuePaused, CanSteer: state.CanSteer, Settings: state.Settings, ToolProgress: state.ToolProgress})
	if string(a.state.Meta["wingman"]) != string(data) {
		meta := map[string]json.RawMessage{"wingman": data}
		a.sessionAction(t.StateAction{Value: &t.SessionMetaChangedAction{Type: t.ActionTypeSessionMetaChanged, Meta: meta}})
	}
}

func (h *ahpHost) message(text string, files, images []string) t.Message {
	message := t.Message{Text: text, Origin: t.MessageOrigin{Kind: t.MessageKindUser}}
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) && h.server.workspace != nil {
			path = filepath.Join(h.server.workspace.RootPath, path)
		}
		uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
		message.Attachments = append(message.Attachments, t.MessageAttachment{Value: &t.MessageResourceAttachment{Type: "resource", Uri: uri, Label: file}})
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

func (a *ahpSession) syncQueue(c *sessionController) {
	next := map[string]t.PendingMessage{}
	kinds := map[string]t.PendingMessageKind{}
	for _, input := range c.state.PendingInputs {
		if input.Origin == "task" || input.State == "active" {
			continue
		}
		kind := t.PendingMessageKindQueued
		if input.Intent == string(code.TurnInputSteer) {
			kind = t.PendingMessageKindSteering
		}
		message := a.host.message(input.Text, input.Files, input.Images)
		meta, _ := json.Marshal(map[string]any{"state": input.State, "position": input.Position})
		message.Meta = map[string]json.RawMessage{"wingman": meta}
		next[input.ID] = t.PendingMessage{Id: input.ID, Message: message}
		kinds[input.ID] = kind
	}
	previous := append([]t.PendingMessage{}, a.chat.QueuedMessages...)
	if a.chat.SteeringMessage != nil {
		previous = append(previous, *a.chat.SteeringMessage)
	}
	for _, old := range previous {
		if _, exists := next[old.Id]; !exists {
			kind := t.PendingMessageKindQueued
			if a.chat.SteeringMessage != nil && a.chat.SteeringMessage.Id == old.Id {
				kind = t.PendingMessageKindSteering
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
