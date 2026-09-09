package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func loopTestToolOutput(id string) string {
	return fmt.Sprintf(`{"type":"function_call","id":"fc_%s","call_id":"call_%s","name":"probe","arguments":"{}","status":"completed"}`, id, id)
}

func TestSendCancellationStopsRemainingTools(t *testing.T) {
	for _, effect := range []tool.Effect{tool.EffectMutates, tool.EffectReadOnly} {
		t.Run(string(effect), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				return phaseTestResponse(false, loopTestToolOutput("first"), loopTestToolOutput("second"))
			})
			var executions atomic.Int64
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 2, MaxParallelTools: 1,
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(effect), Execute: func(context.Context, map[string]any) (tool.Result, error) {
						executions.Add(1)
						cancel()
						return tool.Text("done"), nil
					}}}
				},
			}}
			stream, err := a.Send(ctx, []Content{{Text: "run both tools"}})
			if err != nil {
				t.Fatal(err)
			}
			var turnErr error
			for _, err := range stream {
				turnErr = errors.Join(turnErr, err)
			}
			if !errors.Is(turnErr, context.Canceled) || executions.Load() != 1 || requests != 1 {
				t.Fatalf("error=%v executions=%d requests=%d; expected cancellation after the first tool", turnErr, executions.Load(), requests)
			}
			terminal := a.Events[len(a.Events)-1]
			if terminal.Type != EventTurnTerminal || terminal.Terminal.Status != RuntimeInterrupted || a.Running() {
				t.Fatalf("turn did not finish interrupted: %+v", terminal)
			}
		})
	}
}

func TestProcessToolCallsCancellationDuringNotification(t *testing.T) {
	a := &Agent{Config: &Config{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	executions := 0
	err := a.processToolCalls(ctx, []ToolCall{{ID: "call", Name: "probe"}}, []tool.Tool{{
		Name: "probe",
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("done"), nil
		},
	}}, func(Message, error) bool {
		cancel()
		return true
	})
	if !errors.Is(err, context.Canceled) || executions != 0 {
		t.Fatalf("error=%v executions=%d; executor started after cancellation", err, executions)
	}
}

func TestSendConsumesInputAcceptedDuringStopHook(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(fmt.Sprintf("block=%t", block), func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if requests == 2 && (!strings.Contains(string(body), "queued prompt") || !strings.Contains(string(body), "hook guidance")) {
					t.Errorf("next request omitted the accepted input or its hook context: %s", body)
				}
				return phaseTestResponse(false, commentaryOutput, finalAnswerOutput)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 3}}
			var prompts []string
			a.Hooks.UserPromptSubmit = []hook.UserPromptSubmit{func(_ context.Context, prompt string) (hook.Outcome, error) {
				prompts = append(prompts, prompt)
				return hook.Outcome{AdditionalContext: []string{"hook guidance"}}, nil
			}}
			stopHooks := 0
			a.Hooks.Stop = []hook.Stop{func(_ context.Context, last string, active bool) (hook.Outcome, error) {
				stopHooks++
				if last != "Checked and fixed." || active != (block && stopHooks > 1) {
					t.Errorf("Stop hook received last=%q active=%t", last, active)
				}
				if stopHooks == 1 {
					if !a.QueueInputWithID([]Content{{Text: "queued prompt"}}, "steer-id") {
						t.Error("input during Stop hook was rejected")
					}
					return hook.Outcome{Block: block}, nil
				}
				return hook.Outcome{}, nil
			}}
			a.Recorder = EventRecorderFunc(func(events []RuntimeEvent) error {
				for _, event := range events {
					if event.Type == EventTurnTerminal && a.QueueInput([]Content{{Text: "too late"}}) {
						t.Error("accepted input after the final queue check")
					}
				}
				return nil
			})
			stream, err := a.Send(t.Context(), []Content{{Text: "original prompt"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != 2 || stopHooks != 2 || !slices.Equal(prompts, []string{"original prompt", "queued prompt"}) {
				t.Fatalf("requests=%d Stop hooks=%d prompts=%v", requests, stopHooks, prompts)
			}
			if !slices.ContainsFunc(a.Messages, func(m Message) bool { return m.InputID == "steer-id" }) {
				t.Error("queued input identity was lost")
			}
		})
	}
}

func TestSendQueuedPromptHooksAlsoApplyDuringCleanup(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupt=%t", interrupt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				return phaseTestResponse(false, finalAnswerOutput)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
			var prompts []string
			a.Hooks.UserPromptSubmit = []hook.UserPromptSubmit{func(_ context.Context, prompt string) (hook.Outcome, error) {
				prompts = append(prompts, prompt)
				if prompt == "blocked prompt" {
					return hook.Outcome{Block: true, Reason: "blocked by test hook"}, nil
				}
				return hook.Outcome{AdditionalContext: []string{"hook guidance"}}, nil
			}}
			ctx = WithStreamEventHandlers(ctx, StreamEventHandlers{Commit: func() {
				for _, content := range []Content{{Text: "blocked prompt"}, {Text: "allowed prompt"}, {Text: "background notice", Hidden: true}} {
					if !a.QueueInput([]Content{content}) {
						t.Error("input was not accepted")
					}
				}
				if interrupt {
					cancel()
				}
			}})
			stream, err := a.Send(ctx, []Content{{Text: "original prompt"}})
			if err != nil {
				t.Fatal(err)
			}
			var turnErr error
			for _, err := range stream {
				turnErr = errors.Join(turnErr, err)
			}
			if turnErr == nil || !strings.Contains(turnErr.Error(), "blocked by test hook") || requests != 1 {
				t.Fatalf("error=%v requests=%d; expected prompt rejection before another request", turnErr, requests)
			}
			if interrupt && !errors.Is(turnErr, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", turnErr)
			}
			if !slices.Equal(prompts, []string{"original prompt", "blocked prompt", "allowed prompt"}) {
				t.Fatalf("prompt hooks = %v; hidden notices must not invoke user hooks", prompts)
			}
			for _, messages := range [][]Message{a.Messages, a.requestMessages()} {
				var retained []string
				for _, message := range messages {
					retained = append(retained, contentText(message.Content))
				}
				if slices.Contains(retained, "blocked prompt") || !slices.Contains(retained, "allowed prompt") || !slices.Contains(retained, "background notice") {
					t.Fatalf("retained input after rejection = %v", retained)
				}
			}
		})
	}
}

func TestSendContentFilterStopsToolsAndAutomaticContinuation(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(fmt.Sprintf("steer=%t", steer), func(t *testing.T) {
			requests, executions, stopHooks := 0, 0, 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				if requests > 1 {
					return phaseTestResponse(false, finalAnswerOutput)
				}
				stream := phaseTestResponse(false, loopTestToolOutput("filtered"))
				stream = strings.ReplaceAll(stream, "response.completed", "response.incomplete")
				return strings.ReplaceAll(stream, `"response":{`, `"response":{"incomplete_details":{"reason":"content_filter"},`)
			})
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 2,
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						executions++
						return tool.Text("done"), nil
					}}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
					stopHooks++
					return hook.Outcome{Block: requests == 1}, nil
				}}},
			}}
			ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Commit: func() {
				if steer && requests == 1 && !a.QueueInput([]Content{{Text: "new user direction"}}) {
					t.Error("new user input was rejected")
				}
			}})
			stream, err := a.Send(ctx, []Content{{Text: "start"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			wantRequests := 1
			if steer {
				wantRequests++
			}
			if requests != wantRequests || executions != 0 || stopHooks != wantRequests-1 {
				t.Fatalf("requests=%d executions=%d Stop hooks=%d; filtered output must not drive more work", requests, executions, stopHooks)
			}
		})
	}
}

func TestSendDoesNotCompactAfterReachingTurnLimit(t *testing.T) {
	requests, compactions := 0, 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return strings.ReplaceAll(phaseTestResponse(false, finalAnswerOutput), `"input_tokens":1`, `"input_tokens":10`)
	})
	a := &Agent{Config: &Config{
		client: &client, MaxTurns: 1, ContextWindow: 10, ReserveTokens: 1,
		Hooks: hook.Hooks{
			Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
				return hook.Outcome{Block: true}, nil
			}},
			PreCompact: []hook.PreCompact{func(context.Context, string) (hook.Outcome, error) {
				compactions++
				return hook.Outcome{}, nil
			}},
		},
	}}
	stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnErr error
	for _, err := range stream {
		turnErr = errors.Join(turnErr, err)
	}
	if !errors.Is(turnErr, ErrMaxTurnsExceeded) || requests != 1 || compactions != 0 {
		t.Fatalf("error=%v requests=%d compactions=%d", turnErr, requests, compactions)
	}
}
