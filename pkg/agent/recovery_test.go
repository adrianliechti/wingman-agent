package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestIsRecoverableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"pre-output stream failure", &streamFailure{err: errors.New("connection reset")}, true},
		{"post-output stream failure", &streamFailure{err: errors.New("connection reset"), outputStarted: true}, true},
		{"in-band server error", &responseFailure{code: "server_error"}, true},
		{"in-band rate limit", &responseFailure{code: "rate_limit_exceeded"}, true},
		{"in-band vector timeout", &responseFailure{code: "vector_store_timeout"}, true},
		{"in-band error after output", &responseFailure{code: "server_error", outputStarted: true}, true},
		{"in-band invalid prompt", &responseFailure{code: "invalid_prompt"}, false},
		{"plain protocol failure", errors.New("invalid response item"), false},
		{"bad request", &openai.Error{StatusCode: 400}, false},
		{"request timeout", &openai.Error{StatusCode: 408}, true},
		{"conflict", &openai.Error{StatusCode: 409}, true},
		{"rate limit", &openai.Error{StatusCode: 429}, true},
		{"server error", &openai.Error{StatusCode: 500}, true},
		{"context overflow", &openai.Error{StatusCode: 400, Code: "context_length_exceeded"}, true},
		{"in-band context overflow", &responseFailure{code: "context_length_exceeded"}, true},
		{"in-band Anthropic overflow", &responseFailure{code: "invalid_request_error", message: "prompt is too long"}, true},
		{"encrypted content rejected", &openai.Error{StatusCode: 400, Code: "invalid_encrypted_content"}, true},
		{"thinking signature rejected", &responseFailure{code: "invalid_request_error", message: "Invalid `signature` in `thinking` block"}, true},
		{"unrelated signature", &openai.Error{StatusCode: 400, Message: "invalid signature on request"}, false},
		{"output limit configuration", &openai.Error{StatusCode: 400, Message: "max_output_tokens exceeds the maximum allowed"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRecoverableError(tc.err); got != tc.want {
				t.Fatalf("isRecoverableError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairToolHistoryPreservesParallelCallsAndKnownResults(t *testing.T) {
	a := &Agent{Config: &Config{}, Messages: []Message{
		{Role: RoleUser, Content: []Content{{Text: "update and verify"}}},
		reasoningMessage("r", "m", "opaque"),
		{Role: RoleAssistant, Content: []Content{
			{Text: "Updating two files"},
			{ToolCall: &ToolCall{ID: "known", Name: "write"}},
			{ToolCall: &ToolCall{ID: "missing", Name: "write"}},
		}},
		{Role: RoleAssistant, Content: []Content{
			{ToolResult: &ToolResult{ID: "known", Content: "file A updated"}},
			{ToolResult: &ToolResult{ID: "orphan", Content: "unmatched"}},
			{Text: "Independent observed evidence"},
		}},
		{Role: RoleUser, Content: []Content{{Text: "continue after interruption"}}},
	}}
	if err := a.repairToolHistory(); err != nil {
		t.Fatal(err)
	}
	items := toInput(a.requestMessages())
	var calls, outputs []string
	for _, item := range items {
		if item.OfReasoning != nil {
			t.Fatal("rewrite retained bound reasoning")
		}
		if call := item.OfFunctionCall; call != nil {
			calls = append(calls, call.CallID)
			if len(outputs) != 0 {
				t.Fatal("inserted a result between parallel calls")
			}
		}
		if result := item.OfFunctionCallOutput; result != nil {
			outputs = append(outputs, result.CallID.Value)
			if result.CallID.Value == "missing" && !strings.Contains(result.Output.OfString.Value, "side effects are unknown") {
				t.Fatal("missing result was presented as proof of non-execution")
			}
		}
	}
	encoded, _ := json.Marshal(items)
	if len(calls) != 2 || len(outputs) != 2 || strings.Contains(string(encoded), "unmatched") || !strings.Contains(string(encoded), "file A updated") || !strings.Contains(string(encoded), "Independent observed evidence") {
		t.Fatalf("repair lost evidence or left invalid pairs: %s", encoded)
	}
	if err := a.repairToolHistory(); err != nil || a.ContextRevision != 1 {
		t.Fatalf("repair is not idempotent: revision=%d err=%v", a.ContextRevision, err)
	}
	if len(a.MessagesSnapshot()) != 5 || a.MessagesSnapshot()[1].Content[0].Reasoning.Content != "opaque" {
		t.Fatal("repair changed canonical history")
	}
}

func toolRoundMessages(n, resultBytes int) []Message {
	var messages []Message
	for range n {
		messages = append(messages,
			Message{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: "c", Name: "shell", Args: "{}"}}}},
			Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c", Name: "shell", Content: strings.Repeat("x", resultBytes)}}}},
		)
	}
	return messages
}

func reasoningMessage(id, model, content string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Reasoning: &Reasoning{ID: id, Summary: "s", Content: content, Model: model}}}}
}

func TestDropForeignReasoningPurgesAllPayloadsAfterCrossProviderRewrite(t *testing.T) {
	a := &Agent{Config: &Config{}}
	a.Messages = []Message{
		reasoningMessage("r1", "gpt-5.5", "blob-a"),
		{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{ID: "c1", Name: "shell"}}}},
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: "ok"}}}},
		reasoningMessage("r2", "claude-sonnet-5", "blob-b"),
		{Role: RoleAssistant, Content: []Content{{Text: "done"}}},
	}

	if err := a.dropIncompatibleReasoning(&request{model: "claude-sonnet-5", messages: a.requestMessages()}); err != nil {
		t.Fatal(err)
	}

	projected := a.requestMessages()
	foreign := projected[0].Content[0].Reasoning
	if foreign.Content != "" || foreign.Model != "" {
		t.Fatalf("expected foreign payload purged, got content=%q model=%q", foreign.Content, foreign.Model)
	}
	if foreign.Summary != "s" {
		t.Fatal("expected summary preserved for display")
	}

	native := projected[3].Content[0].Reasoning
	if native.Content != "" || native.Model != "" {
		t.Fatalf("expected later native payload purged after prefix rewrite, got content=%q model=%q", native.Content, native.Model)
	}
	if native.Summary != "s" {
		t.Fatal("expected native summary preserved for display")
	}

	if a.ContextRevision != 1 {
		t.Fatalf("expected context revision bump, got %d", a.ContextRevision)
	}
	if a.Messages[0].Content[0].Reasoning.Content != "blob-a" {
		t.Fatal("foreign reasoning was removed from canonical history")
	}
	if a.Messages[3].Content[0].Reasoning.Content != "blob-b" {
		t.Fatal("native reasoning was removed from canonical history")
	}

	if err := a.dropIncompatibleReasoning(&request{model: "claude-sonnet-5", messages: a.requestMessages()}); err != nil {
		t.Fatal(err)
	}
	if a.ContextRevision != 1 {
		t.Fatal("expected no context revision bump when nothing changes")
	}
}

func TestReplaceContextDropsReplayableReasoningAfterSameModelRewrite(t *testing.T) {
	a := &Agent{Config: &Config{}}
	messages := []Message{
		{Role: RoleUser, Content: []Content{{Text: "rewritten summary"}}},
		reasoningMessage("r1", "claude-fable-5-1", "bound-thinking"),
		{Role: RoleAssistant, Content: []Content{{Text: "answer"}}},
	}

	if err := a.replaceContext("compact model context", messages); err != nil {
		t.Fatal(err)
	}

	projected := a.requestMessages()
	reasoning := projected[1].Content[0].Reasoning
	if reasoning.Content != "" || reasoning.Model != "" {
		t.Fatalf("recovery checkpoint retained replayable reasoning: %+v", reasoning)
	}
	if reasoning.Summary != "s" {
		t.Fatal("recovery checkpoint dropped the reasoning summary")
	}
	if messages[1].Content[0].Reasoning.Content != "bound-thinking" {
		t.Fatal("replaceContext mutated its caller's messages")
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
