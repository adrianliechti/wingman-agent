package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const commentaryOutput = `{"type":"message","id":"msg_progress","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"I will check that now.","annotations":[]}]}`
const finalAnswerOutput = `{"type":"message","id":"msg_final","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Checked and fixed.","annotations":[]}]}`

func phaseTestResponse(itemsInDoneEvents bool, items ...string) string {
	output := "[" + strings.Join(items, ",") + "]"
	var stream strings.Builder
	if itemsInDoneEvents {
		for i, item := range items {
			fmt.Fprintf(&stream, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":%d,\"output_index\":%d,\"item\":%s}\n\n", i+1, i, item)
		}
		output = "[]"
	}
	fmt.Fprintf(&stream, "data: {\"type\":\"response.completed\",\"sequence_number\":%d,\"response\":{\"output\":%s,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", len(items)+1, output)
	return stream.String()
}

func TestSendContinuesAfterCommentary(t *testing.T) {
	for _, doneEvents := range []bool{false, true} {
		t.Run(fmt.Sprintf("items_in_done_events=%t", doneEvents), func(t *testing.T) {
			requests, toolRuns, stopHooks, commits := 0, 0, 0, 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				var body struct {
					Input []struct {
						Role  string `json:"role"`
						Phase string `json:"phase"`
					} `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				users, commentary := 0, 0
				for _, item := range body.Input {
					if item.Role == "user" {
						users++
						if item.Phase != "" {
							t.Error("user input has an assistant phase")
						}
					}
					if item.Phase == "commentary" {
						commentary++
					}
				}
				if users != 1 || commentary != min(requests-1, 2) {
					t.Errorf("request %d: users=%d commentary=%d; expected original input and preserved progress phases", requests, users, commentary)
				}
				if stopHooks != 0 {
					t.Error("stop hook ran before the final answer")
				}
				switch requests {
				case 1:
					return phaseTestResponse(doneEvents, commentaryOutput)
				case 2:
					return phaseTestResponse(doneEvents, strings.ReplaceAll(commentaryOutput, "msg_progress", "msg_progress_2"))
				case 3:
					return phaseTestResponse(doneEvents, `{"type":"function_call","id":"fc_check","call_id":"call_check","name":"check","arguments":"{}","status":"completed"}`)
				default:
					return phaseTestResponse(doneEvents, finalAnswerOutput)
				}
			})
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 4,
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						toolRuns++
						return tool.Text("ok"), nil
					}}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, last string, _ bool) (hook.Outcome, error) {
					stopHooks++
					if last != "Checked and fixed." {
						t.Errorf("stop hook received progress instead of a final answer: %q", last)
					}
					return hook.Outcome{}, nil
				}}},
			}}
			ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Commit: func() {
				commits++
				if !a.Running() {
					t.Error("turn became idle while committing a response")
				}
			}})
			stream, err := a.Send(ctx, []Content{{Text: "Check and fix it"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != 4 || toolRuns != 1 || stopHooks != 1 || commits != 4 || a.Running() {
				t.Fatalf("requests=%d tools=%d stop hooks=%d commits=%d running=%t; expected one complete turn through the final answer", requests, toolRuns, stopHooks, commits, a.Running())
			}

			// A saved session must retain the same phases on the next request.
			data, err := json.Marshal(a.StateSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			var state State
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			restored := &Agent{}
			if err := restored.Restore(state); err != nil {
				t.Fatal(err)
			}
			var phases []string
			for _, item := range toInput(restored.requestMessages()) {
				if item.OfOutputMessage != nil {
					phases = append(phases, string(item.OfOutputMessage.Phase))
				}
			}
			if !slices.Equal(phases, []string{"commentary", "commentary", "final_answer"}) {
				t.Fatalf("phases after session restore = %v", phases)
			}
		})
	}
}

func TestSendCommentaryRespectsMaxTurns(t *testing.T) {
	requests := 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return phaseTestResponse(false, commentaryOutput)
	})
	a := &Agent{Config: &Config{client: &client, MaxTurns: 3}}
	stream, err := a.Send(t.Context(), []Content{{Text: "Check it"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnErr error
	for _, err := range stream {
		turnErr = errors.Join(turnErr, err)
	}
	if !errors.Is(turnErr, ErrMaxTurnsExceeded) || requests != 3 || a.Running() {
		t.Fatalf("error=%v requests=%d running=%t; expected the turn limit to stop repeated commentary", turnErr, requests, a.Running())
	}
}

func TestSendStopsAtFinalAnswerOrUnphasedMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []string
	}{
		{"final", []string{finalAnswerOutput}},
		{"commentary then final", []string{commentaryOutput, finalAnswerOutput}},
		{"unphased", []string{strings.ReplaceAll(finalAnswerOutput, `"phase":"final_answer",`, "")}},
		{"null phase", []string{strings.ReplaceAll(finalAnswerOutput, `"phase":"final_answer"`, `"phase":null`)}},
		{"refusal", []string{strings.ReplaceAll(commentaryOutput, `"type":"output_text","text":"I will check that now.","annotations":[]`, `"type":"refusal","refusal":"Cannot help with that."`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				return phaseTestResponse(false, tc.output...)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != 1 {
				t.Fatalf("requests=%d; expected a completed answer to stop", requests)
			}
		})
	}
}

func TestSendCommentaryPreservesIncompleteHandling(t *testing.T) {
	for _, tc := range []struct {
		reason       string
		wantRequests int
	}{
		{"content_filter", 1},
		{"max_output_tokens", 2},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				stream := phaseTestResponse(false, commentaryOutput)
				stream = strings.ReplaceAll(stream, "response.completed", "response.incomplete")
				return strings.ReplaceAll(stream, `"response":{`, fmt.Sprintf(`"response":{"incomplete_details":{"reason":%q},`, tc.reason))
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 3}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != tc.wantRequests {
				t.Fatalf("requests=%d, want %d", requests, tc.wantRequests)
			}
		})
	}
}

func TestSendCommentaryCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests := 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return phaseTestResponse(false, commentaryOutput)
	})
	a := &Agent{Config: &Config{client: &client, MaxTurns: 3}}
	ctx = WithStreamEventHandlers(ctx, StreamEventHandlers{Commit: cancel})
	stream, err := a.Send(ctx, []Content{{Text: "Check it"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnErr error
	for _, err := range stream {
		turnErr = errors.Join(turnErr, err)
	}
	if !errors.Is(turnErr, context.Canceled) || requests != 1 || a.Running() {
		t.Fatalf("error=%v requests=%d running=%t; expected cancellation after commentary", turnErr, requests, a.Running())
	}
}
