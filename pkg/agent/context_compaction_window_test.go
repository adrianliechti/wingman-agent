package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestCompactionKeepsInterleavedToolCallsPaired(t *testing.T) {
	for _, interleaved := range []struct {
		name    string
		message Message
	}{
		{"text", Message{Role: RoleAssistant, Content: []Content{{Text: "also inspect the other file"}}}},
		{"reasoning", Message{Role: RoleAssistant, Content: []Content{{Reasoning: &Reasoning{Summary: "inspect the other file too"}}}}},
		{"notice", hiddenContextMessage("additional guidance")},
	} {
		for _, large := range []bool{false, true} {
			name := interleaved.name + "/fits"
			if large {
				name = interleaved.name + "/exceeds_window"
			}
			t.Run(name, func(t *testing.T) {
				summaries, continuations := 0, 0
				client := streamingTestClient(func(r *http.Request) string {
					var req struct {
						Input []struct {
							Type   string `json:"type"`
							CallID string `json:"call_id"`
						} `json:"input"`
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(body, &req); err != nil {
						t.Fatal(err)
					}
					pending := map[string]bool{}
					calls := 0
					for _, item := range req.Input {
						switch item.Type {
						case "function_call":
							pending[item.CallID] = true
							calls++
						case "function_call_output":
							if !pending[item.CallID] {
								t.Errorf("request contains result %s without its call", item.CallID)
							}
							delete(pending, item.CallID)
						}
					}
					if len(pending) != 0 {
						t.Errorf("request contains calls without results: %v", pending)
					}
					isSummary := strings.Contains(string(body), summaryRequest[:40])
					wantCalls := 0
					if isSummary == large {
						wantCalls = 2
					}
					if calls != wantCalls {
						t.Errorf("summary=%t: got %d calls, want %d", isSummary, calls, wantCalls)
					}
					if isSummary {
						summaries++
						return phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "Earlier work recorded."))
					}
					continuations++
					return phaseTestResponse(false, finalAnswerOutput)
				})
				args := `{}`
				if large {
					args = `{"padding":"` + strings.Repeat("x", 12_000) + `"}`
				}
				first := ToolCall{ID: "first", Name: "read", Args: args}
				second := ToolCall{ID: "second", Name: "read", Args: `{}`}
				a := &Agent{Config: &Config{client: &client, ContextWindow: 6000, ReserveTokens: 500}, Messages: []Message{
					{Role: RoleUser, Content: []Content{{Text: "inspect both files"}}},
					{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("earlier progress ", 2000)}}},
					{Role: RoleAssistant, Content: []Content{{Text: "read both files now"}}},
					toolCallMessage(first), interleaved.message, toolCallMessage(second),
					toolResultMessage(first, tool.Text("first observation")),
					toolResultMessage(second, tool.Text("second observation")),
					{Role: RoleAssistant, Content: []Content{{Text: "inspection complete"}}},
				}}
				stream, err := a.Send(t.Context(), []Content{{Text: "continue"}})
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range stream {
					if err != nil {
						t.Fatal(err)
					}
				}
				if summaries != 1 || continuations != 1 || a.ContextRevision != 1 {
					t.Fatalf("summaries=%d continuations=%d revision=%d", summaries, continuations, a.ContextRevision)
				}
			})
		}
	}
}

func TestCompactionFallsBackWithoutWindow(t *testing.T) {
	for _, tc := range []struct {
		name          string
		userTokens    int
		olderTokens   int
		summaryTokens int
		wantSources   []bool
		wantError     bool
	}{
		{"latest input needs space", 6500, 20_000, 20, []bool{true}, false},
		{"checkpoint needs space", 6000, 20_000, 1000, []bool{false, true}, false},
		{"window prevents reduction", 10, 800, 900, []bool{false, true}, false},
		{"fallback still cannot fit", 6000, 20_000, 4000, []bool{false, true}, true},
		{"latest input cannot fit", 9500, 20_000, 20, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sources []bool
			client := streamingTestClient(func(r *http.Request) string {
				var req struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				input := string(req.Input)
				if !strings.Contains(input, summaryRequest[:40]) {
					t.Fatal("expected a summary request")
				}
				sources = append(sources, strings.Contains(input, "RECENT_EVIDENCE"))
				return phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", strings.Repeat("fact", tc.summaryTokens)))
			})
			latest := Message{Role: RoleUser, InputID: "latest", Content: []Content{{Text: strings.Repeat("task", tc.userTokens)}}}
			a := &Agent{Config: &Config{client: &client, ContextWindow: 10_000, ReserveTokens: 1000}, Messages: []Message{
				latest,
				{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("old ", tc.olderTokens)}}},
				{Role: RoleAssistant, Content: []Content{{Text: "RECENT_EVIDENCE " + strings.Repeat("new ", 2500)}}},
			}}
			before := a.contextSnapshot()
			req := &request{messages: a.requestMessages()}
			a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 10_000}})
			err := a.prepareRequest(t.Context(), req)
			if (err != nil) != tc.wantError {
				t.Fatalf("prepareRequest error=%v, wantError=%t", err, tc.wantError)
			}
			if !slices.Equal(sources, tc.wantSources) {
				t.Errorf("summary inputs containing recent evidence=%v, want %v", sources, tc.wantSources)
			}
			if !messagesEqual(a.MessagesSnapshot(), before) {
				t.Fatal("compaction changed canonical messages")
			}
			if tc.wantError {
				if a.ContextRevision != 0 || !messagesEqual(a.requestMessages(), before) {
					t.Fatal("failed compaction changed the working context")
				}
				return
			}
			if a.ContextRevision != 1 || a.requestTokens(req) > int64(a.contextInputBudget(req.model)) || messagesTokens(req.messages) >= messagesTokens(before) {
				t.Fatal("compaction did not produce a smaller context within budget")
			}
			if i := lastVisibleUserIndex(req.messages); i < 0 || !messagesEqual(req.messages[i:i+1], []Message{latest}) {
				t.Fatal("latest user input was changed or removed")
			}
			checkpoint := req.messages[len(req.messages)-1]
			if !isCompactionSummary(checkpoint) || strings.Contains(contentText(checkpoint.Content), windowContinuation) {
				t.Fatal("fallback did not install a checkpoint without a recent window")
			}
		})
	}
}

func TestCompactionWithLeadingGuidanceRecoversOverflow(t *testing.T) {
	for _, reactive := range []bool{false, true} {
		name := "measured_usage"
		if reactive {
			name = "provider_overflow"
		}
		t.Run(name, func(t *testing.T) {
			summaries, requests := 0, 0
			client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var req struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				input := string(req.Input)
				status, contentType := http.StatusOK, "text/event-stream"
				body := phaseTestResponse(false, finalAnswerOutput)
				if strings.Contains(input, summaryRequest[:40]) {
					summaries++
					if !strings.Contains(input, "recorded progress") {
						t.Error("fallback summary omitted the assistant history")
					}
					body = phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "Earlier work recorded."))
				} else {
					requests++
					if reactive && requests == 1 {
						status, contentType = http.StatusBadRequest, "application/json"
						body = `{"error":{"code":"context_length_exceeded","message":"context limit reached"}}`
					} else if !strings.Contains(input, summaryPrefix) || !strings.Contains(input, "current guidance") || strings.Contains(input, "recorded progress") {
						t.Error("continuation did not retain guidance and replace the assistant history with a checkpoint")
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}))
			a := &Agent{Config: &Config{
				client: &client, ContextWindow: 6000, ReserveTokens: 500,
				ContextInstructions: func() string { return "current guidance" },
			}, Messages: []Message{
				hiddenContextMessage(sessionContextPrefix + "current guidance"),
				{Role: RoleUser, Content: []Content{{Text: "task"}}},
				{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("recorded progress ", 250)}}},
			}}
			wantRequests := 2
			if !reactive {
				a.anchorContextUsage(&request{messages: a.requestMessages()}, &response{usage: Usage{InputTokens: 6000}})
				wantRequests = 1
			}
			stream, err := a.Send(t.Context(), []Content{{Text: "continue"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if summaries != 1 || requests != wantRequests || a.ContextRevision != 1 {
				t.Fatalf("summaries=%d requests=%d revision=%d", summaries, requests, a.ContextRevision)
			}
		})
	}
}
