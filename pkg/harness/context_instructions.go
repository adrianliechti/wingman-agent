package harness

import (
	"slices"
	"strings"
)

const sessionContextPrefix = "[Session context]\nThis is the complete current session context. It replaces all earlier session context messages; omitted sections no longer apply.\n\n"

const sessionContextDeltaPrefix = "[Session context]\nThis is an incremental session context update. Sections below replace their earlier versions; unlisted sections remain in effect.\n\n"

func isSessionContext(m Message) bool {
	if m.Role != RoleUser || !m.Hidden {
		return false
	}
	text := contentText(m.Content)
	return strings.HasPrefix(text, sessionContextPrefix) || strings.HasPrefix(text, sessionContextDeltaPrefix)
}

func lastSessionContextIndex(messages []Message) int {
	for i, message := range slices.Backward(messages) {
		if isSessionContext(message) {
			return i
		}
	}
	return -1
}

// fullSessionContextMessage expands an incremental delta message into a
// self-contained [Session context] snapshot when compaction or compression
// drops earlier snapshots from the active context window.
func fullSessionContextMessage(m Message) Message {
	cloned := cloneMessage(m)
	if cloned.ContextSections == nil && !strings.HasPrefix(contentText(cloned.Content), sessionContextDeltaPrefix) {
		return cloned
	}
	full := sessionContextPrefix + formatContextSections(cloned.ContextSections)
	cloned.Content = []Content{{Text: full}}
	return cloned
}

func normalizeContextSections(in []ContextSection) []ContextSection {
	out := make([]ContextSection, 0, len(in))
	for _, s := range in {
		title := strings.TrimSpace(s.Title)
		content := strings.TrimSpace(s.Content)
		if content == "" {
			continue
		}
		out = append(out, ContextSection{Title: title, Content: content})
	}
	return out
}

func formatContextSections(sections []ContextSection) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if s.Content == "" {
			continue
		}
		if s.Title != "" {
			parts = append(parts, "# "+s.Title+"\n\n"+s.Content)
		} else {
			parts = append(parts, s.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Updates are appended at a normal request boundary, leaving the cached prefix
// untouched. Structured sections emit compact deltas when only a subset of
// sections changed; compaction re-expands the latest state losslessly.
func (a *Agent) syncContextInstructions() error {
	if update, changed := a.contextInstructionsUpdate(a.requestMessages()); changed {
		return a.appendMessages(update)
	}
	return nil
}

func (a *Agent) contextInstructionsUpdate(messages []Message) (Message, bool) {
	if a.ContextSections != nil {
		return a.contextSectionsUpdate(messages)
	}
	current := ""
	if a.ContextInstructions != nil {
		current = a.ContextInstructions()
	}
	previous := lastSessionContextIndex(messages)
	if previous >= 0 && contentText(messages[previous].Content) == sessionContextPrefix+current {
		return Message{}, false
	}
	if previous < 0 && current == "" {
		return Message{}, false
	}
	// Like Codex's project/environment fragments, this is contextual user
	// input. Mid-history system messages are rejected by some providers.
	return Message{
		Role: RoleUser, Hidden: true,
		Content: []Content{{Text: sessionContextPrefix + current}},
	}, true
}

func (a *Agent) contextSectionsUpdate(messages []Message) (Message, bool) {
	current := normalizeContextSections(a.ContextSections())
	previous := lastSessionContextIndex(messages)
	if previous < 0 {
		if len(current) == 0 {
			return Message{}, false
		}
		return Message{
			Role:            RoleUser,
			Hidden:          true,
			Content:         []Content{{Text: sessionContextPrefix + formatContextSections(current)}},
			ContextSections: current,
		}, true
	}

	prevMsg := messages[previous]
	if prevMsg.ContextSections == nil {
		full := formatContextSections(current)
		if contentText(prevMsg.Content) == sessionContextPrefix+full {
			return Message{}, false
		}
		return Message{
			Role:            RoleUser,
			Hidden:          true,
			Content:         []Content{{Text: sessionContextPrefix + full}},
			ContextSections: current,
		}, true
	}

	prevSections := prevMsg.ContextSections
	if slices.Equal(prevSections, current) {
		return Message{}, false
	}

	prevMap := make(map[string]string, len(prevSections))
	for _, s := range prevSections {
		prevMap[s.Title] = s.Content
	}
	currMap := make(map[string]string, len(current))
	var changed []ContextSection
	for _, s := range current {
		currMap[s.Title] = s.Content
		if old, ok := prevMap[s.Title]; !ok || old != s.Content {
			changed = append(changed, s)
		}
	}
	var removed []string
	for _, s := range prevSections {
		if _, ok := currMap[s.Title]; !ok && s.Title != "" {
			removed = append(removed, s.Title)
		}
	}

	var body strings.Builder
	body.WriteString(sessionContextDeltaPrefix)
	if len(removed) > 0 {
		body.WriteString("Omitted sections (no longer apply): " + strings.Join(removed, ", ") + ".")
		if len(changed) > 0 {
			body.WriteString("\n\n")
		}
	}
	body.WriteString(formatContextSections(changed))

	return Message{
		Role:            RoleUser,
		Hidden:          true,
		Content:         []Content{{Text: body.String()}},
		ContextSections: current,
	}, true
}
