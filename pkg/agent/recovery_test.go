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

func reasoningMessage(id, model, content string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Reasoning: &Reasoning{ID: id, Summary: "s", Content: content, Model: model}}}}
}

func TestDropForeignReasoningPurgesOtherModels(t *testing.T) {
	a := &Agent{Config: &Config{}}
	a.Messages = []Message{
		reasoningMessage("r1", "gpt-5.5", "blob-a"),
		{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: "c1", Name: "shell"}}}},
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: "ok"}}}},
		reasoningMessage("r2", "claude-sonnet-5", "blob-b"),
		{Role: RoleAssistant, Content: []Content{{Text: "done"}}},
	}

	a.dropForeignReasoning("claude-sonnet-5")

	foreign := a.Messages[0].Content[0].Reasoning
	if foreign.Content != "" || foreign.Model != "" {
		t.Fatalf("expected foreign payload purged, got content=%q model=%q", foreign.Content, foreign.Model)
	}
	if foreign.Summary != "s" {
		t.Fatal("expected summary preserved for display")
	}

	native := a.Messages[3].Content[0].Reasoning
	if native.Content != "blob-b" || native.Model != "claude-sonnet-5" {
		t.Fatalf("expected native payload kept, got content=%q model=%q", native.Content, native.Model)
	}

	if a.Revision != 1 {
		t.Fatalf("expected revision bump, got %d", a.Revision)
	}

	a.dropForeignReasoning("claude-sonnet-5")
	if a.Revision != 1 {
		t.Fatal("expected no revision bump when nothing changes")
	}
}

func TestDropDanglingReasoning(t *testing.T) {
	reasoning := reasoningMessage("r1", "m", "blob")
	toolCall := Message{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: "c1", Name: "shell"}}}}
	toolResult := Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: "ok"}}}}
	text := Message{Role: RoleAssistant, Content: []Content{{Text: "answer"}}}
	user := Message{Role: RoleUser, Content: []Content{{Text: "hi"}}}

	cases := []struct {
		name     string
		messages []Message
		want     int
		changed  bool
	}{
		{"kept before tool call", []Message{reasoning, toolCall, toolResult}, 3, false},
		{"kept before text", []Message{reasoning, text}, 2, false},
		{"chain kept before text", []Message{reasoning, reasoningMessage("r2", "m", "b2"), text}, 3, false},
		{"dropped at end", []Message{user, toolCall, toolResult, reasoning}, 3, true},
		{"dropped before user", []Message{reasoning, user}, 1, true},
		{"dropped before tool result", []Message{reasoning, toolResult}, 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := dropDanglingReasoning(tc.messages)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			if len(out) != tc.want {
				t.Fatalf("got %d messages, want %d", len(out), tc.want)
			}
			for _, m := range out {
				for _, c := range m.Content {
					if c.Reasoning != nil && tc.changed {
						t.Fatal("dangling reasoning message survived")
					}
				}
			}
		})
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
