package server

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Native commands are supplied by the selected session. Compactor is a host
// control operation, not a prompt command, and is deliberately not synthesized
// here: arbitrary AHP clients must be able to send every suggested command.
func (b *backendRuntime) promptCommands(sessionID string) []SkillEntry {
	provider, ok := b.agent.(code.CommandProvider)
	if !ok {
		return nil
	}
	var commands []SkillEntry
	seen := map[string]bool{}
	for _, command := range provider.Commands(sessionID) {
		name := strings.TrimPrefix(command.Name, "/")
		key := strings.ToLower(name)
		if name == "" || strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '/' }) >= 0 || seen[key] {
			continue
		}
		seen[key] = true
		commands = append(commands, SkillEntry{Name: name, Description: command.Description, InputHint: command.InputHint})
	}
	return commands
}

func (b *backendRuntime) isPromptCommand(sessionID, text string) bool {
	name, ok := skill.ParseSlashCommand(text)
	if !ok {
		return false
	}
	for _, command := range b.promptCommands(sessionID) {
		if strings.EqualFold(command.Name, name) {
			return true
		}
	}
	return false
}

func (h *ahpHost) completions(raw json.RawMessage) (t.CompletionsResult, error) {
	result := t.CompletionsResult{Items: []t.CompletionItem{}}
	var p struct {
		Channel string               `json:"channel"`
		Kind    t.CompletionItemKind `json:"kind"`
		Text    *string              `json:"text"`
		Offset  *int64               `json:"offset"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Text == nil || p.Offset == nil || p.Kind == "" {
		return result, ahpInvalid("completion channel, kind, text and offset required")
	}
	text := utf16.Encode([]rune(*p.Text))
	if *p.Offset < 0 || *p.Offset > int64(len(text)) {
		return result, ahpInvalid("offset must be within text")
	}
	offset := int(*p.Offset)
	if offset > 0 && offset < len(text) && utf16.DecodeRune(rune(text[offset-1]), rune(text[offset])) != unicode.ReplacementChar {
		return result, ahpInvalid("offset must be a UTF-16 character boundary within text")
	}
	_, ch, err := h.ensureSession(p.Channel)
	if err != nil {
		return result, err
	}
	if ch == nil {
		return result, ahpInvalid("chat channel required")
	}
	if p.Kind != t.CompletionItemKindUserMessage {
		return result, nil
	}
	start, end := offset, offset
	for start > 0 && !unicode.IsSpace(rune(text[start-1])) {
		start--
	}
	for end < len(text) && !unicode.IsSpace(rune(text[end])) {
		end++
	}
	if start == offset || text[start] != '/' || strings.ContainsRune(string(utf16.Decode(text[start+1:end])), '/') {
		return result, nil
	}
	query := strings.ToLower(string(utf16.Decode(text[start+1 : offset])))
	b := ch.controller.backend
	if !b.beginOperation() {
		return result, ahpInvalid("workspace closed")
	}
	defer b.operations.Done()
	seen := map[string]bool{}
	appendItem := func(entry SkillEntry, kind string) {
		key := strings.ToLower(entry.Name)
		if seen[key] {
			return
		}
		seen[key] = true
		if !strings.Contains(key, query) && !strings.Contains(strings.ToLower(entry.Description), query) {
			return
		}
		// VS Code's slash-command convention; skills are text-dispatched commands too.
		meta := map[string]json.RawMessage{}
		meta["command"], _ = json.Marshal(entry.Name)
		if entry.Description != "" {
			meta["description"], _ = json.Marshal(entry.Description)
		}
		if entry.InputHint != "" {
			meta["argumentHint"], _ = json.Marshal(entry.InputHint)
		}
		if kind == "skill" {
			meta["isSkill"] = json.RawMessage("true")
		}
		result.Items = append(result.Items, t.CompletionItem{
			InsertText: "/" + entry.Name,
			RangeStart: ahpPtr(int64(start)), RangeEnd: ahpPtr(int64(end)),
			Attachment: t.MessageAttachment{Value: &t.SimpleMessageAttachment{
				Type: t.MessageAttachmentKindSimple, Label: "/" + entry.Name,
				DisplayKind: &kind, Meta: meta,
			}},
		})
	}
	// Native commands take precedence at the beginning of a message. Inline
	// mentions remain skills, and qualified plugin names remain independently
	// reachable even when a native command has the same bare name.
	if start == 0 {
		if _, ok := b.agent.(code.Compactor); ok {
			appendItem(SkillEntry{Name: "compact", Description: "Summarize the conversation to free context", InputHint: "focus"}, "command")
		}
		for _, command := range b.promptCommands(ch.controller.ref.SessionID) {
			appendItem(command, "command")
		}
	}
	for _, entry := range h.server.skillCatalogue() {
		appendItem(entry, "skill")
	}
	return result, nil
}
