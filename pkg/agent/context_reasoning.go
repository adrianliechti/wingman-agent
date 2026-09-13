package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

func reasoningPrefix(req *request) string {
	tools, _ := json.Marshal(toTools(req.tools))
	encoded, _ := json.Marshal([]string{req.instructions, string(tools)})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// Removing one bound block may invalidate later blocks. A single checkpoint
// clears all opaque payloads while preserving summaries and canonical history.
func (a *Agent) dropIncompatibleReasoning(req *request) error {
	prefix := reasoningPrefix(req)
	for _, m := range req.messages {
		for _, c := range m.Content {
			r := c.Reasoning
			if r != nil && r.Content != "" && (r.Model != req.model || (r.Prefix != "" && r.Prefix != prefix)) {
				return a.replaceContext("drop reasoning bound to another model or request prefix", req.messages)
			}
		}
	}
	return nil
}

func (a *Agent) dropRejectedReasoning() (bool, error) {
	messages := a.requestMessages()
	for _, m := range messages {
		for _, c := range m.Content {
			if reasoningToInput(c.Reasoning) != nil {
				return true, a.replaceContext("drop rejected reasoning signatures", messages)
			}
		}
	}
	return false, nil
}

// dropDanglingReasoning removes reasoning-only messages that are not followed
// by assistant output (text or a tool call) from the same turn. Providers
// reject replayed reasoning items whose required following item is missing —
// which happens when a broken stream or orphaned-tool-call cleanup strands one.
func dropDanglingReasoning(messages []Message) ([]Message, bool) {
	followedByOutput, changed := false, false
	var kept []Message
	for _, m := range slices.Backward(messages) {
		reasoningOnly := m.Role == RoleAssistant && len(m.Content) > 0
		hasOutput := false
		for _, c := range m.Content {
			reasoningOnly = reasoningOnly && c.Reasoning != nil
			hasOutput = hasOutput || c.ToolCall != nil || c.Text != "" || c.Refusal != ""
		}
		if reasoningOnly && !followedByOutput {
			changed = true
			continue
		}
		if !reasoningOnly {
			followedByOutput = m.Role == RoleAssistant && hasOutput
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	slices.Reverse(kept)
	return kept, true
}

// clearReplayableReasoning mutates an owned snapshot, never the live history.
func clearReplayableReasoning(messages []Message) {
	for i := range messages {
		for j := range messages[i].Content {
			reasoning := messages[i].Content[j].Reasoning
			if reasoning == nil {
				continue
			}
			reasoning.Content = ""
			reasoning.Model = ""
			reasoning.Prefix = ""
		}
	}
}
