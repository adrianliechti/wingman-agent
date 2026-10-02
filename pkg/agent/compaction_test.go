package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestSendCompactionHookLifecycle(t *testing.T) {
	for _, reactive := range []bool{false, true} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("reactive=%t/stop=%t", reactive, stop), func(t *testing.T) {
				requests, summaries := 0, 0
				client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{
					Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						var request struct {
							Stream bool            `json:"stream"`
							Input  json.RawMessage `json:"input"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						status, contentType := http.StatusOK, "text/event-stream"
						var body string
						if strings.Contains(string(request.Input), summaryRequest[:40]) {
							summaries++
							body = phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "summary of earlier work"))
						} else {
							requests++
							contentType = "text/event-stream"
							body = phaseTestResponse(false, finalAnswerOutput)
							if requests == 1 {
								if reactive {
									status, contentType = http.StatusBadRequest, "application/json"
									body = `{"error":{"code":"context_length_exceeded","message":"context limit reached"}}`
								} else {
									body = strings.ReplaceAll(body, `"input_tokens":1`, `"input_tokens":6000`)
								}
							} else if !strings.Contains(string(request.Input), "summary of earlier work") || !strings.Contains(string(request.Input), "compacted session guidance") {
								t.Errorf("next request omitted the summary or compact-session hook context: %s", request.Input)
							}
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					}),
				}))
				var order []string
				a := &Agent{Config: &Config{
					client: &client, MaxTurns: 3, ContextWindow: 6000, ReserveTokens: 500,
					Hooks: hook.Hooks{
						SessionStart: []hook.SessionStart{func(_ context.Context, source string) (hook.Outcome, error) {
							order = append(order, source)
							if source == "compact" {
								return hook.Outcome{AdditionalContext: []string{"compacted session guidance"}}, nil
							}
							return hook.Outcome{}, nil
						}},
						PreCompact: []hook.PreCompact{func(context.Context, string) (hook.Outcome, error) {
							order = append(order, "pre")
							return hook.Outcome{}, nil
						}},
						PostCompact: []hook.PostCompact{func(context.Context, string) (hook.Outcome, error) {
							order = append(order, "post")
							return hook.Outcome{Stop: stop}, nil
						}},
						Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
							return hook.Outcome{Block: requests == 1}, nil
						}},
					},
				}, Messages: []Message{
					{Role: RoleUser, Content: []Content{{Text: "earlier work"}}},
					{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("earlier progress ", 1000)}}},
				}}
				stream, err := a.Send(t.Context(), []Content{{Text: "continue the work"}})
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range stream {
					if err != nil {
						t.Fatal(err)
					}
				}
				wantOrder := []string{"startup", "pre", "post", "compact"}
				wantRequests, wantStatus := 2, RuntimeCompleted
				if stop {
					wantOrder = wantOrder[:3]
					wantRequests, wantStatus = 1, RuntimeInterrupted
				}
				if !slices.Equal(order, wantOrder) || summaries != 1 || requests != wantRequests {
					t.Fatalf("hook order=%v summaries=%d requests=%d", order, summaries, requests)
				}
				if terminal := a.Events[len(a.Events)-1].Terminal; terminal.Status != wantStatus {
					t.Fatalf("terminal = %+v, want %s", terminal, wantStatus)
				}
			})
		}
	}
}

func TestShouldCompactProactivelyScalesReserveOnLargeWindows(t *testing.T) {
	a := &Agent{Config: &Config{}}

	// 1M window: a fixed 32k reserve would defer compaction to 968k (~97%);
	// the 10% floor triggers at 900k instead.
	if a.compactionOvershoot("claude-opus-4-8", 899_000) > 0 {
		t.Error("should not compact below the 10% headroom on a 1M window")
	}
	if a.compactionOvershoot("claude-opus-4-8", 901_000) <= 0 {
		t.Error("should compact within 10% of a 1M window")
	}

	// 200k window: the 64k generation allowance exceeds the 32k baseline
	// and 10% margin, so compaction must leave room for the full response.
	if a.compactionOvershoot("claude-opus-4-5", 136_000) > 0 {
		t.Error("should not compact while the full output allowance still fits")
	}
	if a.compactionOvershoot("claude-opus-4-5", 136_001) <= 0 {
		t.Error("should compact when the full output allowance no longer fits")
	}

	// A model limited to 32k output needs only the existing 32k baseline.
	if a.compactionOvershoot("gpt-5.3-codex-spark", 96_000) > 0 || a.compactionOvershoot("gpt-5.3-codex-spark", 96_001) <= 0 {
		t.Error("should reserve 32k for a 128k model with a 32k output allowance")
	}
}

func TestShouldCompactProactivelyHonorsExplicitReserve(t *testing.T) {
	a := &Agent{Config: &Config{ReserveTokens: 10_000}}

	// With an explicit 10k reserve the threshold is 990k, not the 900k the
	// window fraction would impose — 905k must not trigger compaction.
	if a.compactionOvershoot("claude-opus-4-8", 905_000) > 0 {
		t.Error("explicit reserve must be honored, not inflated by the window fraction")
	}
	if a.compactionOvershoot("claude-opus-4-8", 995_000) <= 0 {
		t.Error("explicit reserve still triggers once its threshold is crossed")
	}
}

func TestShouldCompactProactivelyUsesOutputTokenBudgetOverride(t *testing.T) {
	for _, tc := range []struct {
		budget     int
		inputLimit int64
	}{
		{budget: 0, inputLimit: 208_000},
		{budget: 96_000, inputLimit: 176_000},
		{budget: 256_000, inputLimit: 144_000}, // Clamped to the model's 128k output limit.
		{budget: 16_000, inputLimit: 240_000},  // Keep the 32k minimum context reserve.
	} {
		a := &Agent{Config: &Config{ContextWindow: 272_000, OutputTokenBudget: tc.budget}}
		if a.compactionOvershoot("gpt-6-astra", tc.inputLimit) > 0 || a.compactionOvershoot("gpt-6-astra", tc.inputLimit+1) <= 0 {
			t.Errorf("output budget %d should trigger compaction above %d input tokens", tc.budget, tc.inputLimit)
		}
	}
}

func TestShouldCompactProactivelyDisabled(t *testing.T) {
	if (&Agent{Config: &Config{ContextWindow: -1}}).compactionOvershoot("claude-opus-4-8", 999_999_999) > 0 {
		t.Error("a negative context window disables proactive compaction")
	}
	if (&Agent{Config: &Config{}}).compactionOvershoot("claude-opus-4-8", 0) > 0 {
		t.Error("zero measured tokens must not trigger compaction")
	}
}

func TestRetainedUsersNewestFirstAndVerbatim(t *testing.T) {
	first := Message{Role: RoleUser, InputID: "first", Content: []Content{{Text: strings.Repeat("old", 1000)}}}
	second := Message{Role: RoleUser, InputID: "second", Content: []Content{{Text: "keep 日本語 and exact whitespace \n "}}}
	third := Message{Role: RoleUser, InputID: "third", Content: []Content{{Text: "use this image"}, {File: &File{Data: "data:image/png;base64,AAAA"}}}}
	messages := []Message{first, hiddenContextMessage(summaryPrefix + "\nold checkpoint"), second, {Role: RoleAssistant, Content: []Content{{Text: "done"}}}, third}
	retained := retainedUserMessages(messages, messageTokens(second)+messageTokens(third))
	if len(retained) != 2 || retained[0].InputID != "second" || retained[1].InputID != "third" {
		t.Fatalf("retained messages = %+v", retained)
	}
	if retained[0].Content[0].Text != second.Content[0].Text || retained[1].Content[1].File.Data != third.Content[1].File.Data {
		t.Fatal("user input was changed")
	}
}

func TestCompactionRejectsOversizedLatestInputWithoutChangingIt(t *testing.T) {
	input := Message{Role: RoleUser, Content: []Content{{Text: strings.Repeat("request ", 1000)}}}
	_, err := compactionUserMessages([]Message{input}, 100, true)
	if err == nil || !strings.Contains(err.Error(), "conversation preserved") {
		t.Fatalf("oversized input result=%v", err)
	}
	if len(input.Content[0].Text) != 8000 {
		t.Fatal("latest input was shortened")
	}
}

func TestSessionFactsRestateLedgerEditsAndChecks(t *testing.T) {
	edit := func(position uint64, path string) RuntimeEvent {
		return reviewResult(position, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: path, AfterExists: true}}})
	}
	check := func(position uint64, command string, exit int) RuntimeEvent {
		return reviewResult(position, "exec_command", fmt.Sprintf(`{"command":%q,"validation":true}`, command), map[string]any{"exit_code": exit})
	}
	a := &Agent{Config: &Config{}, Events: []RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "one"}, edit(2, "a.go"), check(3, "go test ./...", 0),
		{Type: EventTurnStarted, TurnID: "two"}, edit(5, "b.go"), edit(6, "a.go"), check(7, "go vet   ./...", 1),
		{Type: EventTurnStarted, TurnID: "three"}, edit(9, "reverted.go"),
		{Type: EventTurnUndo, TurnID: "three"},
	}}

	want := "\n\nRecorded by the harness:" +
		"\n- Files changed in this session: a.go, b.go" +
		"\n- Check `go test ./...`: passed before later edits (exit 0)" +
		"\n- Check `go vet ./...`: failed (exit 1)"
	if got := a.sessionFacts(); got != want {
		t.Fatalf("sessionFacts =\n%s\nwant\n%s", got, want)
	}
	if got := (&Agent{Config: &Config{}}).sessionFacts(); got != "" {
		t.Fatalf("facts without ledger work = %q", got)
	}
}

func TestCompactionReportsOversizedCheckpointBeforeUserInput(t *testing.T) {
	_, err := compactionUserMessages([]Message{{Role: RoleUser, Content: []Content{{Text: "hi"}}}}, -1, true)
	if err == nil || !strings.Contains(err.Error(), "summary or session context") {
		t.Fatalf("misleading compaction error: %v", err)
	}
}

func TestCompactionWithoutReductionStopsBeforeSendingOrSummarizingAgain(t *testing.T) {
	requests := 0
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(req.Input), summaryRequest[:40]) {
			t.Fatal("sent main request after ineffective compaction")
		}
		body := phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", strings.Repeat("larger briefing ", 100)))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}))
	a := &Agent{Config: &Config{client: &client, ContextWindow: 6000, ReserveTokens: 500}, Messages: []Message{
		{Role: RoleUser, Content: []Content{{Text: "original task"}}},
		{Role: RoleAssistant, Content: []Content{{Text: "recorded progress"}}},
	}}
	// A measured input can be much larger than a text estimate on some
	// providers. Exercise proactive pressure without a huge fake transcript.
	a.anchorContextUsage(&request{messages: a.requestMessages()}, &response{usage: Usage{InputTokens: 6000}})
	stream, err := a.Send(t.Context(), []Content{{Text: "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnErr error
	for _, err := range stream {
		if err != nil {
			turnErr = err
		}
	}
	if requests != 1 || turnErr == nil || !strings.Contains(turnErr.Error(), "did not reduce") || a.ContextRevision != 0 || len(a.MessagesSnapshot()) != 3 {
		t.Fatalf("requests=%d err=%v revision=%d", requests, turnErr, a.ContextRevision)
	}
}

func TestRetainedWindowStartsAtStepBoundary(t *testing.T) {
	user := func(text string) Message { return Message{Role: RoleUser, Content: []Content{{Text: text}}} }
	reply := func(text string) Message { return Message{Role: RoleAssistant, Content: []Content{{Text: text}}} }
	call := func(id string) Message {
		return toolCallMessage(ToolCall{ID: id, Name: "read", Args: `{"file_path":"a.go"}`})
	}
	result := func(id string) Message {
		return toolResultMessage(ToolCall{ID: id, Name: "read"}, tool.Text("observed"))
	}
	messages := []Message{
		hiddenContextMessage(summaryPrefix + "\nold checkpoint"), // 0
		user("task"),           // 1
		reply("plan"),          // 2
		call("c1"), call("c2"), // 3, 4
		result("c1"), result("c2"), // 5, 6
		{Role: RoleUser, Hidden: true, Content: []Content{{Text: sessionContextPrefix + "guidance"}}}, // 7
		reply("done"), // 8
	}
	cost := func(from, to int) int { return messagesTokens(messages[from:to]) }
	for _, tc := range []struct {
		name   string
		budget int
		want   int
	}{
		{"everything after the checkpoint", cost(1, 9), 1},
		{"never the checkpoint itself", cost(0, 9) * 2, 1},
		{"tool batch stays with its results", cost(3, 7) + cost(8, 9), 8},
		{"a boundary before the batch", cost(2, 7) + cost(8, 9), 2},
		{"last reply does not fit", cost(8, 9) - 1, len(messages)},
	} {
		if got := retainedWindowStart(messages, tc.budget); got != tc.want {
			t.Errorf("%s: start = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := recentWindow(messages[1:], 7-1); len(got) != 8 || !isSessionContext(got[6]) {
		t.Fatalf("latest snapshot removed from window: %+v", got)
	}
	if got := recentWindow(messages[1:], -1); len(got) != 7 || isSessionContext(got[6]) {
		t.Fatalf("superseded snapshot kept in window: %+v", got)
	}
}

func TestCompactionKeepsRecentWindowVerbatim(t *testing.T) {
	const summary = "SUMMARY_OF_OLDER_WORK"
	requests := 0
	client := streamingTestClient(func(r *http.Request) string {
		requests++
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		input := string(req.Input)
		if strings.Contains(input, summaryRequest[:40]) {
			if !strings.Contains(input, "earlier progress") || strings.Contains(input, "next step: verify") {
				t.Error("summarizer must see the older history and not the recent window")
			}
			return phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", summary))
		}
		if !strings.Contains(input, `"function_call_output"`) || strings.Index(input, summaryPrefix) > strings.Index(input, "next step: verify") {
			t.Error("recent tool work must follow the checkpoint verbatim")
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})
	observed := strings.Repeat("observed line\n", 300)
	a := &Agent{Config: &Config{client: &client, ContextWindow: 40_000, ReserveTokens: 2_000}, Messages: []Message{
		{Role: RoleUser, Content: []Content{{Text: "original task"}}},
		{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("earlier progress ", 5000)}}},
		{Role: RoleUser, Content: []Content{{Text: "next step: verify"}}},
		toolCallMessage(ToolCall{ID: "c1", Name: "read", Args: `{"file_path":"a.go"}`}),
		toolResultMessage(ToolCall{ID: "c1", Name: "read"}, tool.Text(observed)),
		{Role: RoleAssistant, Content: []Content{{Text: "recent progress"}}},
	}}
	a.anchorContextUsage(&request{messages: a.requestMessages()}, &response{usage: Usage{InputTokens: 40_000}})
	stream, err := a.Send(t.Context(), []Content{{Text: "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 || a.ContextRevision != 1 {
		t.Fatalf("requests=%d revision=%d", requests, a.ContextRevision)
	}
	got := a.requestMessages()
	text := func(i int) string { return contentText(got[i].Content) }
	if len(got) != 8 || text(0) != "original task" || !isCompactionSummary(got[1]) || text(2) != "next step: verify" ||
		got[3].Content[0].ToolCall == nil || got[4].Content[0].ToolResult == nil || got[4].Content[0].ToolResult.Content != observed ||
		text(5) != "recent progress" || text(6) != "continue" || got[7].Role != RoleAssistant {
		for i := range got {
			t.Logf("%d %s hidden=%t %q", i, got[i].Role, got[i].Hidden, strings.Repeat(" ", 0)+summarizeForLog(got[i]))
		}
		t.Fatal("compacted layout differs from [older user, checkpoint, recent window]")
	}
	if checkpoint := text(1); !strings.Contains(checkpoint, summary) || !strings.Contains(checkpoint, windowContinuation) || strings.Contains(checkpoint, "earlier progress") {
		t.Fatalf("checkpoint = %q", checkpoint)
	}
}

func summarizeForLog(m Message) string {
	s := contentText(m.Content)
	if len(m.Content) > 0 && m.Content[0].ToolCall != nil {
		s = "call " + m.Content[0].ToolCall.ID
	}
	if len(m.Content) > 0 && m.Content[0].ToolResult != nil {
		s = "result " + m.Content[0].ToolResult.ID
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
