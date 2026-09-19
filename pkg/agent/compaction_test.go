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
						status, contentType := http.StatusOK, "application/json"
						var body string
						if !request.Stream {
							summaries++
							body = `{"output":[` + strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "summary of earlier work") + `]}`
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
	_, err := compactionUserMessages([]Message{input}, 100)
	if err == nil || !strings.Contains(err.Error(), "conversation preserved") {
		t.Fatalf("oversized input result=%v", err)
	}
	if len(input.Content[0].Text) != 8000 {
		t.Fatal("latest input was shortened")
	}
}

func TestCompactionReportsOversizedCheckpointBeforeUserInput(t *testing.T) {
	_, err := compactionUserMessages([]Message{{Role: RoleUser, Content: []Content{{Text: "hi"}}}}, -1)
	if err == nil || !strings.Contains(err.Error(), "summary or session context") {
		t.Fatalf("misleading compaction error: %v", err)
	}
}

func TestCompactionWithoutReductionStopsBeforeSendingOrSummarizingAgain(t *testing.T) {
	requests := 0
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Stream {
			t.Fatal("sent main request after ineffective compaction")
		}
		body := `{"status":"completed","output":[` + strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", strings.Repeat("larger briefing ", 100)) + `]}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
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
