package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestAskAnswersFromContextWithoutChangingIt(t *testing.T) {
	var body struct {
		Instructions string
		ToolChoice   string `json:"tool_choice"`
		Tools        []struct{ Name string }
		Input        []struct {
			Type    string
			CallID  string `json:"call_id"`
			Output  json.RawMessage
			Content json.RawMessage
		}
	}
	client := streamingTestClient(func(r *http.Request) string {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})

	// A turn in progress: one call has returned, the other still runs.
	a := &Agent{
		Config: &Config{
			client:       &client,
			Model:        func() string { return "m" },
			Instructions: func() string { return "session instructions" },
			Tools:        func() []tool.Tool { return []tool.Tool{{Name: "check"}} },
		},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: "Fix the build."}}},
			{Role: RoleAssistant, Content: []Content{
				{ToolCall: &ToolCall{ID: "done", Name: "check", Args: "{}"}},
				{ToolCall: &ToolCall{ID: "running", Name: "check", Args: "{}"}},
			}},
			{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "done", Name: "check", Content: tool.TextContent("ok")}}}},
		},
	}
	before, revision := a.StateVersion()

	var streamed strings.Builder
	answer, err := a.Ask(t.Context(), "  why that file?  ", func(text string) { streamed.WriteString(text) })
	if err != nil || answer != "Checked and fixed." || streamed.String() != answer {
		t.Fatalf("answer = %q streamed = %q err = %v", answer, streamed.String(), err)
	}

	if body.ToolChoice != "none" || body.Instructions != "session instructions" || len(body.Tools) != 1 || body.Tools[0].Name != "check" {
		t.Errorf("the request must keep the cached prefix and disable calls: %+v", body)
	}
	outputs := map[string]string{}
	for _, item := range body.Input {
		if item.Type == "function_call_output" {
			outputs[item.CallID] = string(item.Output)
		}
	}
	if len(outputs) != 2 || !strings.Contains(outputs["running"], "has not returned yet") || !strings.Contains(outputs["done"], "ok") {
		t.Errorf("call outputs = %v, want the running call closed for this request", outputs)
	}
	if last := string(body.Input[len(body.Input)-1].Content); !strings.Contains(last, "Side question") || !strings.Contains(last, "why that file?") {
		t.Errorf("last input = %s, want the side question", last)
	}

	if count, after := a.StateVersion(); count != before || len(a.MessagesSnapshot()) != 3 {
		t.Fatalf("the side question changed the conversation: %d messages, revision %d → %d", count, revision, after)
	}
	if usage := a.UsageSnapshot(); usage.InputTokens != 1 || usage.OutputTokens != 1 {
		t.Errorf("usage = %+v, want the side question counted as session cost", usage)
	}

	if _, err := a.Ask(t.Context(), "  ", nil); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("empty question error = %v", err)
	}
}

func TestAskFallsBackToBriefingWhenTheHistoryIsNotReplayable(t *testing.T) {
	requests := 0
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var body struct {
			Stream       bool
			Instructions string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Stream {
			// The provider ignored tool_choice and produced no answer.
			output := phaseTestResponse(false, `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: r, Body: io.NopCloser(strings.NewReader(output))}, nil
		}
		if !strings.Contains(body.Instructions, "why that file?") {
			t.Errorf("briefing instructions = %q", body.Instructions)
		}
		output := `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Because the build needs it."}]}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(strings.NewReader(output))}, nil
	})}))
	a := &Agent{
		Config:   &Config{client: &client, Model: func() string { return "m" }},
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "Fix the build."}}}},
	}

	answer, err := a.Ask(t.Context(), "why that file?", nil)
	if err != nil || answer != "Because the build needs it." || requests != 2 {
		t.Fatalf("answer = %q requests = %d err = %v", answer, requests, err)
	}
	if len(a.MessagesSnapshot()) != 1 {
		t.Fatal("the side question changed the conversation")
	}
}

func TestAskDuringARunningTurnLeavesTheTurnUndisturbed(t *testing.T) {
	var side struct {
		outputs map[string]string
	}
	turnRequests := 0
	client := streamingTestClient(func(r *http.Request) string {
		var body struct {
			ToolChoice string `json:"tool_choice"`
			Input      []struct {
				Type   string
				CallID string `json:"call_id"`
				Output json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ToolChoice == "none" {
			side.outputs = map[string]string{}
			for _, item := range body.Input {
				if item.Type == "function_call_output" {
					side.outputs[item.CallID] = string(item.Output)
				}
			}
			return phaseTestResponse(false, `{"type":"message","id":"msg_side","role":"assistant","status":"completed","content":[{"type":"output_text","text":"It is still checking.","annotations":[]}]}`)
		}
		turnRequests++
		if turnRequests == 1 {
			return phaseTestResponse(true, `{"type":"function_call","id":"fc_slow","call_id":"slow","name":"check","arguments":"{}","status":"completed"}`)
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})

	started, release := make(chan struct{}), make(chan struct{})
	a := &Agent{Config: &Config{
		client: &client,
		Model:  func() string { return "m" },
		Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				close(started)
				<-release
				return tool.Text("checked"), nil
			}}}
		},
	}}

	stream, err := a.Send(t.Context(), []Content{{Text: "Fix the build."}})
	if err != nil {
		t.Fatal(err)
	}
	turn := make(chan error, 1)
	go func() {
		var last error
		for _, err := range stream {
			last = errors.Join(last, err)
		}
		turn <- last
	}()

	<-started
	answer, err := a.Ask(t.Context(), "what is it doing?", nil)
	if err != nil || answer != "It is still checking." {
		t.Fatalf("answer = %q err = %v", answer, err)
	}
	if !strings.Contains(side.outputs["slow"], "has not returned yet") {
		t.Fatalf("side request outputs = %v, want the running call closed", side.outputs)
	}

	close(release)
	if err := <-turn; err != nil {
		t.Fatal(err)
	}
	var transcript []string
	for _, message := range a.MessagesSnapshot() {
		for _, content := range message.Content {
			if content.ToolResult != nil {
				transcript = append(transcript, "result:"+content.ToolResult.Content.Text())
			} else if text := content.AsText(); text != "" {
				transcript = append(transcript, text)
			}
		}
	}
	joined := strings.Join(transcript, " | ")
	if !strings.Contains(joined, "result:checked") || !strings.Contains(joined, "Checked and fixed.") ||
		strings.Contains(joined, "what is it doing?") || strings.Contains(joined, "still checking") || strings.Contains(joined, "not returned yet") {
		t.Fatalf("conversation after the turn = %s", joined)
	}
}
