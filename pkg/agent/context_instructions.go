package agent

import "strings"

const sessionContextPrefix = "[Session context]\nThis is the complete current session context. It replaces all earlier session context messages; omitted sections no longer apply.\n\n"

func isSessionContext(m Message) bool {
	return m.Role == RoleUser && m.Hidden && strings.HasPrefix(contentText(m.Content), sessionContextPrefix)
}

func lastSessionContextIndex(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if isSessionContext(messages[i]) {
			return i
		}
	}
	return -1
}

// Updates are appended at a normal request boundary, leaving the cached prefix
// untouched. The explicit replacement also handles removed memory or guidance.
// The latest snapshot lives in the journal, so restoring does not duplicate it.
func (a *Agent) syncContextInstructions() error {
	if update, changed := a.contextInstructionsUpdate(a.requestMessages()); changed {
		return a.appendMessages(update)
	}
	return nil
}

func (a *Agent) contextInstructionsUpdate(messages []Message) (Message, bool) {
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
