package agent

import (
	"strings"
	"testing"
)

func trimTestAgent(messages []Message) *Agent {
	return &Agent{Config: &Config{}, Messages: messages}
}

func toolExchange(id, result string) []Message {
	return []Message{
		{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: id, Name: "read"}}}},
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: id, Name: "read", Content: result}}}},
	}
}

func TestTrimStaleToolResults(t *testing.T) {
	big := strings.Repeat("x", 8*1024)

	var messages []Message
	messages = append(messages, Message{Role: RoleUser, Content: []Content{{Text: "task"}}})
	for range 20 {
		messages = append(messages, toolExchange("call", big)...)
	}

	a := trimTestAgent(messages)
	freed := a.trimStaleToolResults()
	if freed == 0 {
		t.Fatal("expected bytes freed")
	}
	if a.Revision == 0 {
		t.Fatal("expected revision bump")
	}

	var trimmed, intact int
	for _, m := range a.Messages {
		for _, c := range m.Content {
			if c.ToolResult == nil {
				continue
			}
			if strings.Contains(c.ToolResult.Content, "trimmed to reclaim context") {
				trimmed++
			} else if len(c.ToolResult.Content) == len(big) {
				intact++
			}
		}
	}
	if trimmed == 0 {
		t.Fatal("no tool results were trimmed")
	}
	if intact == 0 {
		t.Fatal("recent tool results must stay intact")
	}

	last := a.Messages[len(a.Messages)-1]
	if got := last.Content[0].ToolResult.Content; len(got) != len(big) {
		t.Fatalf("newest tool result was trimmed (len %d)", len(got))
	}

	if a.trimStaleToolResults() != 0 {
		t.Fatal("second trim should be a no-op")
	}
}

func TestTrimStaleToolResultsDropsImages(t *testing.T) {
	big := strings.Repeat("x", 8*1024)

	var messages []Message
	messages = append(messages, Message{Role: RoleAssistant, Content: []Content{
		{ToolResult: &ToolResult{ID: "img", Name: "view_image", Content: "[image attached below]"}},
		{File: &File{Data: strings.Repeat("A", 64*1024)}},
	}})
	for range 20 {
		messages = append(messages, toolExchange("call", big)...)
	}

	a := trimTestAgent(messages)
	if a.trimStaleToolResults() == 0 {
		t.Fatal("expected bytes freed")
	}

	first := a.Messages[0]
	for _, c := range first.Content {
		if c.File != nil {
			t.Fatal("old image data must be dropped")
		}
	}
	if !strings.Contains(first.Content[0].ToolResult.Content, "image result trimmed") {
		t.Fatalf("image tool result = %q", first.Content[0].ToolResult.Content)
	}
}

func TestTrimStaleToolResultsProtectsSmallSessions(t *testing.T) {
	a := trimTestAgent(toolExchange("call", strings.Repeat("x", 8*1024)))
	if freed := a.trimStaleToolResults(); freed != 0 {
		t.Fatalf("freed %d bytes from a small session", freed)
	}
}
