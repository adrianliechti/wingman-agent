package harness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"

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
		{"in-band slow down", &responseFailure{code: "slow_down"}, true},
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
		// Gateways forward the upstream provider's own transient codes.
		{"in-band Anthropic overload", &responseFailure{code: "overloaded_error"}, true},
		{"in-band Anthropic api error", &responseFailure{code: "api_error"}, true},
		{"in-band Anthropic rate limit", &responseFailure{code: "rate_limit_error"}, true},
		{"in-band Anthropic timeout", &responseFailure{code: "timeout_error"}, true},
		{"in-band Anthropic overload after output", &responseFailure{code: "overloaded_error", outputStarted: true}, true},
		{"in-band service unavailable", &responseFailure{code: "service_unavailable_error"}, true},
		{"in-band Bedrock throttling", &responseFailure{code: "throttlingException"}, true},
		{"in-band Bedrock model timeout", &responseFailure{code: "ModelTimeoutException"}, true},
		{"in-band Google unavailable", &responseFailure{code: "UNAVAILABLE"}, true},
		{"in-band Google resource exhausted", &responseFailure{code: "RESOURCE_EXHAUSTED"}, true},
		{"in-band status 429", &responseFailure{code: "429"}, true},
		{"in-band status 503", &responseFailure{code: "503"}, true},
		{"in-band status 400", &responseFailure{code: "400"}, false},
		{"in-band status 401", &responseFailure{code: "401"}, false},
		{"in-band number that is no status", &responseFailure{code: "1234"}, false},
		{"in-band authentication", &responseFailure{code: "authentication_error"}, false},
		{"in-band invalid request", &responseFailure{code: "invalid_request_error", message: "tool_choice is invalid"}, false},
		{"in-band content policy", &responseFailure{code: "content_policy_violation"}, false},
		{"in-band model not found", &responseFailure{code: "model_not_found"}, false},
		{"in-band unknown code", &responseFailure{code: "unknown_failure"}, false},
		{"in-band message without code", &responseFailure{message: "upstream closed the response"}, false},
		{"in-band usage limit", &responseFailure{code: "organization_usage_limit_exceeded"}, false},
		// 413 is an overflow by status, with or without a body.
		{"payload too large without body", &openai.Error{StatusCode: 413}, true},
		{"payload too large with quota code", &openai.Error{StatusCode: 413, Code: "insufficient_quota"}, false},
		{"unprocessable without body", &openai.Error{StatusCode: 422}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRecoverableError(tc.err); got != tc.want {
				t.Fatalf("isRecoverableError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContextOverflowWordings(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		want          bool
	}{
		{"Anthropic window", "input length and `max_tokens` exceed context limit: 188240 + 21333 > 200000, decrease input length or `max_tokens` and try again", true},
		{"Anthropic prompt", "prompt is too long: 213462 tokens > 200000 maximum", true},
		{"Gemini", "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575).", true},
		{"xAI", "This model's maximum prompt length is 131072 but the request contains 537812 tokens.", true},
		{"llama.cpp", "the request exceeds the available context size, try increasing it", true},
		{"Bedrock tokens", "Too many input tokens", true},
		{"Bedrock length", "Input is too long for requested model.", true},
		{"OpenAI", "Your input exceeds the context window of this model. Please adjust your input and try again.", true},
		{"OpenRouter", "This endpoint's maximum context length is 200000 tokens. However, you requested about 250000 tokens.", true},

		{"output limit configuration", "max_output_tokens exceeds the maximum allowed", false},
		{"output limit above model maximum", "max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for this model", false},
		{"output count above allowed", "The max_output_tokens value (70000) exceeds the maximum number of tokens allowed (65536).", false},
		{"token rate limit", "This request would exceed the rate limit for your organization of 400,000 input tokens per minute.", false},
		{"unknown context parameter", "Unknown parameter: 'context_limit'.", false},
		{"limit named before the excess", "The context limit option cannot exceed 10.", false},
		{"prompt parameter", "Invalid value for 'prompt_cache_key': maximum length is 64.", false},
		{"image limit", "Too many images: the maximum number of images allowed is 100.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{
				&openai.Error{StatusCode: 400, Message: tc.message},
				&streamFailure{err: &providerError{err: &openai.Error{StatusCode: 400, Code: "invalid_request_error", Message: tc.message}}},
				&responseFailure{code: "invalid_request_error", message: tc.message},
				&responseFailure{message: tc.message},
			} {
				if got := isContextOverflowError(err); got != tc.want {
					t.Errorf("%T: isContextOverflowError() = %v, want %v", err, got, tc.want)
				}
				// Overflow is recovered by compaction; any other input rejection is final.
				if got := isRecoverableError(err); got != tc.want {
					t.Errorf("%T: isRecoverableError() = %v, want %v", err, got, tc.want)
				}
			}
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"413 without body", &openai.Error{StatusCode: 413}, true},
		{"413 with unrelated message", &openai.Error{StatusCode: 413, Message: "Request exceeds the maximum size"}, true},
		{"400 without body", &openai.Error{StatusCode: 400}, false},
		{"overflow wording on a rate limit status", &openai.Error{StatusCode: 429, Message: "too many tokens per minute"}, false},
		{"overflow wording on a server error", &openai.Error{StatusCode: 500, Message: "context window service failed"}, false},
		{"transport failure", &streamFailure{err: errors.New("context window")}, false},
		{"in-band status code", &responseFailure{code: "413"}, false},
		{"in-band throttling that counts tokens", &responseFailure{code: "ThrottlingException", message: "Too many tokens, please wait before trying again."}, false},
		{"in-band rate limit that counts tokens", &responseFailure{code: "rate_limit_error", message: "too many tokens per minute"}, false},
	} {
		if got := isContextOverflowError(tc.err); got != tc.want {
			t.Errorf("%s: isContextOverflowError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTerminalProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"quota", &responseFailure{code: "insufficient_quota"}, true},
		{"usage limit over HTTP", &openai.Error{StatusCode: 429, Code: "organization_usage_limit_exceeded"}, true},
		{"unauthorized", &streamFailure{err: &openai.Error{StatusCode: 401}}, true},
		{"forbidden", &openai.Error{StatusCode: 403}, true},
		{"in-band authentication", &responseFailure{code: "authentication_error"}, true},
		{"content policy", &openai.Error{StatusCode: 400, Code: "content_policy_violation"}, true},
		{"in-band content filter", &responseFailure{code: "content_filter"}, true},
		{"invalid prompt", &responseFailure{code: "invalid_prompt"}, true},
		// These may be an overflow in unrecognized wording or a history item
		// the briefing transcript omits, so compaction still degrades.
		{"invalid request", &openai.Error{StatusCode: 400, Code: "invalid_request_error"}, false},
		{"in-band invalid request", &responseFailure{code: "invalid_request_error"}, false},
		{"model not found", &openai.Error{StatusCode: 404, Code: "model_not_found"}, false},
		{"context overflow", &openai.Error{StatusCode: 400, Code: "context_length_exceeded"}, false},
		{"rate limit", &openai.Error{StatusCode: 429}, false},
		{"server error", &responseFailure{code: "server_error"}, false},
		{"transport failure", &streamFailure{err: errors.New("connection reset")}, false},
		{"local failure", errors.New("summary response was incomplete"), false},
		{"no error", nil, false},
	} {
		if got := isTerminalProviderError(tc.err); got != tc.want {
			t.Errorf("%s: isTerminalProviderError() = %v, want %v", tc.name, got, tc.want)
		}
		if tc.want && isRecoverableError(tc.err) {
			t.Errorf("%s: terminal error is also retryable", tc.name)
		}
	}
}

func TestRetryAllowedBoundsAttempts(t *testing.T) {
	transient := &responseFailure{code: "server_error"}
	if !retryAllowed(transient, 0) || !retryAllowed(transient, maxStreamRetries-1) {
		t.Fatal("transient failure was not retried within the attempt budget")
	}
	if retryAllowed(transient, maxStreamRetries) {
		t.Fatal("retried beyond the attempt budget")
	}
	if retryAllowed(&responseFailure{code: "invalid_prompt"}, 0) {
		t.Fatal("retried a terminal failure")
	}
}

func TestQuotaFailuresAreTerminalAcrossTransports(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded"} {
		for _, err := range []error{
			&responseFailure{code: code},
			&openai.Error{StatusCode: 429, Code: code},
			&streamFailure{err: &openai.Error{StatusCode: 503, Code: code}},
		} {
			if isRecoverableError(err) {
				t.Errorf("retried quota failure %T: %s", err, code)
			}
		}
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
			{ToolResult: &ToolResult{ID: "known", Content: tool.TextContent("file A updated")}},
			{ToolResult: &ToolResult{ID: "orphan", Content: tool.TextContent("unmatched")}},
			{Text: "Independent observed evidence"},
		}},
		{Role: RoleUser, Content: []Content{{Text: "continue after interruption"}}},
	}}
	if err := a.repairToolHistory(false); err != nil {
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
	if err := a.repairToolHistory(false); err != nil || a.ContextRevision != 1 {
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
			Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c", Name: "shell", Content: tool.TextContent(strings.Repeat("x", resultBytes))}}}},
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
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: tool.TextContent("ok")}}}},
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
	toolResult := Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: tool.TextContent("ok")}}}}
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
