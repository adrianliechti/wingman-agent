package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func isRecoverableError(err error) bool {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return true
	}

	switch apiErr.StatusCode {
	case 401, 403:
		return false
	default:
		return true
	}
}

func (a *Agent) removeOrphanedToolMessages() {

	callIDs := make(map[string]bool)
	outputIDs := make(map[string]bool)

	for _, m := range a.Messages {
		for _, c := range m.Content {
			if c.ToolCall != nil && c.ToolCall.ID != "" {
				callIDs[c.ToolCall.ID] = true
			}

			if c.ToolResult != nil && c.ToolResult.ID != "" {
				outputIDs[c.ToolResult.ID] = true
			}
		}
	}

	dropped := make(map[int]bool)
	droppedToolCall := make(map[int]bool)

	for i, m := range a.Messages {
		for _, c := range m.Content {
			if c.ToolCall != nil && !outputIDs[c.ToolCall.ID] {
				dropped[i] = true
				droppedToolCall[i] = true
				break
			}

			if c.ToolResult != nil && !callIDs[c.ToolResult.ID] {
				dropped[i] = true
				break
			}
		}
	}

	// A reasoning item dangling in front of a dropped tool_call will be rejected
	// by the Responses API. Drop any contiguous reasoning-only run immediately
	// preceding a dropped tool_call message.
	for i := range droppedToolCall {
		for j := i - 1; j >= 0; j-- {
			if !isReasoningOnlyMessage(a.Messages[j]) {
				break
			}
			dropped[j] = true
		}
	}

	if len(dropped) == 0 {
		return
	}

	var cleaned []Message
	for i, m := range a.Messages {
		if !dropped[i] {
			cleaned = append(cleaned, m)
		}
	}

	a.Messages = cleaned
}

func isReasoningOnlyMessage(m Message) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, c := range m.Content {
		if c.Reasoning == nil {
			return false
		}
	}
	return true
}

func (a *Agent) compactMessages(ctx context.Context) {
	summaryMessages, recentMessages := splitMessagesForRecoverySummary(a.Messages)

	summary, err := a.summarizeMessages(ctx, summaryMessages)
	if err != nil || summary == "" {
		a.truncateMessagesForRecovery()
		return
	}

	a.Messages = append([]Message{{
		Role:    RoleUser,
		Hidden:  true,
		Content: []Content{{Text: summary}},
	}}, recentMessages...)
	a.removeOrphanedToolMessages()
}

const maxSummarizeBytes = 100 * 1024
const minRecoveryMessagesToPreserve = 12

func splitMessagesForRecoverySummary(messages []Message) ([]Message, []Message) {
	if len(messages) == 0 {
		return nil, nil
	}

	// Preserve the active turn verbatim. This mirrors the handoff/compaction
	// pattern of summarizing prior history while forwarding the newest items.
	split := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser && !messages[i].Hidden {
			split = i
			break
		}
	}

	if split < 0 {
		split = len(messages) - minRecoveryMessagesToPreserve
		if split <= 0 {
			return messages, nil
		}
	}

	return messages[:split], messages[split:]
}

func (a *Agent) summarizeMessages(ctx context.Context, messages []Message) (string, error) {
	transcript := recoverySummaryTranscript(messages)

	if transcript == "" {
		return "", nil
	}

	model := ""
	if a.Config.Model != nil {
		model = a.Model()
	}

	resp, err := a.client.Responses.New(ctx, responses.ResponseNewParams{
		Model: model,
		Instructions: openai.String(
			"Summarize the following earlier conversation between a user and an AI assistant. " +
				"Preserve all important context: what the user asked, what was done, what files were modified, " +
				"key decisions made, and the current state of the task. " +
				"Be concise but complete. Format as a briefing the assistant can use to continue the conversation. " +
				"Do not answer the user's latest request; only summarize the prior context.",
		),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(transcript),
		},
		Store: openai.Bool(false),
	})

	if err != nil {
		return "", err
	}

	var result strings.Builder
	result.WriteString("[Previous conversation summary]\n\n")

	for _, item := range resp.Output {
		msg := item.AsMessage()
		for _, part := range msg.Content {
			text := part.AsOutputText()
			if text.Text != "" {
				result.WriteString(text.Text)
			}
		}
	}

	return result.String(), nil
}

func recoverySummaryTranscript(messages []Message) string {
	var chunks []string
	total := 0

	for i := len(messages) - 1; i >= 0; i-- {
		var mb strings.Builder
		m := messages[i]
		for _, c := range m.Content {
			if c.Text != "" {
				fmt.Fprintf(&mb, "[%s]: %s\n\n", m.Role, truncate(c.Text, 2000))
			}

			if c.Refusal != "" {
				fmt.Fprintf(&mb, "[%s]: %s\n\n", m.Role, truncate(c.Refusal, 2000))
			}

			if c.ToolCall != nil {
				fmt.Fprintf(&mb, "[tool call]: %s(%s)\n\n", c.ToolCall.Name, truncate(c.ToolCall.Args, 200))
			}

			if c.ToolResult != nil {
				fmt.Fprintf(&mb, "[tool result]: %s\n\n", truncate(c.ToolResult.Content, 500))
			}
		}

		if mb.Len() == 0 {
			continue
		}
		// Always include at least the newest non-empty message so a single
		// oversized item (e.g. a giant tool result) doesn't collapse the
		// transcript to empty and trigger the recovery fallback.
		if len(chunks) > 0 && total+mb.Len() > maxSummarizeBytes {
			break
		}

		chunks = append(chunks, mb.String())
		total += mb.Len()
	}

	var sb strings.Builder
	for i := len(chunks) - 1; i >= 0; i-- {
		sb.WriteString(chunks[i])
	}

	return sb.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + " [truncated]"
}

func (a *Agent) truncateMessagesForRecovery() {
	if len(a.Messages) > minRecoveryMessagesToPreserve {
		a.Messages = a.Messages[len(a.Messages)-minRecoveryMessagesToPreserve:]
	}
	a.removeOrphanedToolMessages()
}
