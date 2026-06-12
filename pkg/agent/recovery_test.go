package agent

import (
	"strings"
	"testing"
)

func toolRoundMessages(n, resultBytes int) []Message {
	var messages []Message
	for i := 0; i < n; i++ {
		messages = append(messages,
			Message{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: "c", Name: "shell", Args: "{}"}}}},
			Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c", Name: "shell", Content: strings.Repeat("x", resultBytes)}}}},
		)
	}
	return messages
}

func TestSplitRecoverySummaryLongSingleTask(t *testing.T) {
	messages := append(
		[]Message{{Role: RoleUser, Content: []Content{{Text: "do the task"}}}},
		toolRoundMessages(200, 2048)...,
	)

	summary, recent := splitMessagesForRecoverySummary(messages)

	if len(summary) == 0 {
		t.Fatal("expected non-empty summary side for long single-task session")
	}
	if len(recent) < minRecoveryMessagesToPreserve {
		t.Fatalf("expected at least %d recent messages, got %d", minRecoveryMessagesToPreserve, len(recent))
	}
	if recent[0].Role != RoleUser || recent[0].Content[0].Text != "do the task" {
		t.Fatal("expected the last user message to be preserved verbatim at the start of the recent side")
	}

	total := 0
	for _, m := range recent[1:] {
		total += messageBytes(m)
	}
	if total > maxRecentBytes {
		t.Fatalf("recent side is %d bytes, exceeds budget %d", total, maxRecentBytes)
	}
}

func TestSplitRecoverySummarySmallTailUsesLastUserMessage(t *testing.T) {
	messages := append(
		[]Message{{Role: RoleUser, Content: []Content{{Text: "first task"}}}},
		toolRoundMessages(20, 512)...,
	)
	messages = append(messages, Message{Role: RoleUser, Content: []Content{{Text: "second task"}}})
	messages = append(messages, toolRoundMessages(3, 512)...)

	summary, recent := splitMessagesForRecoverySummary(messages)

	if len(recent) == 0 || recent[0].Role != RoleUser || recent[0].Content[0].Text != "second task" {
		t.Fatalf("expected recent side to start at the last user message, got %d recent messages", len(recent))
	}
	if len(summary) != len(messages)-len(recent) {
		t.Fatalf("split lost messages")
	}
}

func TestSplitRecoverySummaryCountsFileContent(t *testing.T) {
	messages := []Message{{Role: RoleUser, Content: []Content{{Text: "task"}}}}
	for i := 0; i < 20; i++ {
		messages = append(messages, Message{Role: RoleAssistant, Content: []Content{{File: &File{Data: strings.Repeat("a", 8*1024)}}}})
	}

	summary, _ := splitMessagesForRecoverySummary(messages)

	if len(summary) == 0 {
		t.Fatal("expected file content to count toward the recent-side budget")
	}
}

func TestSplitRecoverySummarySmallConversation(t *testing.T) {
	messages := append(
		[]Message{{Role: RoleUser, Content: []Content{{Text: "task"}}}},
		toolRoundMessages(2, 128)...,
	)

	summary, recent := splitMessagesForRecoverySummary(messages)

	if len(summary) != 0 {
		t.Fatalf("expected empty summary side for small conversation, got %d messages", len(summary))
	}
	if len(recent) != len(messages) {
		t.Fatalf("expected all messages on recent side")
	}
}
