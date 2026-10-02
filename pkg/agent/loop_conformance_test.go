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
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type loopWireItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
	Content   []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type loopWireRequest struct {
	Input []loopWireItem `json:"input"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func readLoopRequest(t *testing.T, r *http.Request) loopWireRequest {
	t.Helper()
	var req loopWireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatal(err)
	}
	return req
}

func loopInputOrder(items []loopWireItem) []string {
	var order []string
	for _, item := range items {
		switch item.Type {
		case "function_call":
			order = append(order, "call:"+item.CallID)
		case "function_call_output":
			order = append(order, "result:"+item.CallID)
		default:
			for _, content := range item.Content {
				order = append(order, item.Role+":"+content.Text)
			}
		}
	}
	return order
}

func loopCall(id, name, args string) string {
	return fmt.Sprintf(`{"type":"function_call","id":%q,"call_id":%q,"name":%q,"arguments":%q,"status":"completed"}`, "fc_"+id, id, name, args)
}

func runLoopTurn(ctx context.Context, a *Agent, prompt string) error {
	stream, err := a.Send(ctx, []Content{{Text: prompt}})
	if err != nil {
		return err
	}
	var runErr error
	for _, err := range stream {
		runErr = errors.Join(runErr, err)
	}
	return runErr
}

// Pi agent-loop.test.ts: completion-order notifications, source-order results,
// and steering after a tool batch. Codex tool_parallelism.rs: tool_results_grouped;
// pending_input.rs: steers_during_tool_drain_preserve_tool_output_and_each_input.
func TestLoopBatchOrderingAndQueuedInput(t *testing.T) {
	for _, effect := range []tool.Effect{tool.EffectReadOnly, tool.EffectMutates} {
		t.Run(string(effect), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				client := streamingTestClient(func(r *http.Request) string {
					requests++
					req := readLoopRequest(t, r)
					if requests == 1 {
						return phaseTestResponse(false, loopCall("first", "probe", `{"value":"first"}`), loopCall("second", "probe", `{"value":"second"}`))
					}
					want := []string{"user:start", "call:first", "call:second", "result:first", "result:second", "user:steer one", "user:steer two"}
					if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
						t.Errorf("continuation input = %v, want %v", got, want)
					}
					if req.Input[3].Output != "first completed" || req.Input[4].Output != "second completed" {
						t.Error("real tool output was lost while consuming steering")
					}
					return phaseTestResponse(false, finalAnswerOutput)
				})
				a := &Agent{Config: &Config{client: &client, MaxTurns: 2, ToolTimeout: -1}}
				a.Tools = func() []tool.Tool {
					return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(effect), Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
						value := args["value"].(string)
						if value == "first" {
							for _, text := range []string{"steer one", "steer two"} {
								if !a.QueueInputWithID([]Content{{Text: text}}, text) {
									t.Error("steering was rejected during tool execution")
								}
							}
							// Virtual time guarantees that the second parallel call finishes first.
							time.Sleep(time.Second)
						}
						return tool.Text(value + " completed"), nil
					}}}
				}
				stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
				if err != nil {
					t.Fatal(err)
				}
				var notifications []string
				for m, err := range stream {
					if err != nil {
						t.Fatal(err)
					}
					for _, c := range m.Content {
						if c.ToolResult != nil {
							notifications = append(notifications, c.ToolResult.ID)
						}
					}
				}
				wantNotifications := []string{"first", "second"}
				if effect == tool.EffectReadOnly {
					slices.Reverse(wantNotifications)
				}
				if requests != 2 || !slices.Equal(notifications, wantNotifications) {
					t.Fatalf("requests=%d result notifications=%v, want %v", requests, notifications, wantNotifications)
				}
				var persisted []string
				for _, m := range a.MessagesSnapshot() {
					if m.InputID != "" {
						persisted = append(persisted, m.InputID)
					}
					for _, c := range m.Content {
						if c.ToolResult != nil {
							persisted = append(persisted, c.ToolResult.ID)
						}
					}
				}
				if want := []string{"first", "second", "steer one", "steer two"}; !slices.Equal(persisted, want) {
					t.Fatalf("persisted results and input IDs = %v, want %v", persisted, want)
				}
			})
		})
	}
}

// Codex step_settings.rs: captured_model_replans_tools_and_retains_the_issuing_request_router.
// Pi agent.test.ts: tool loadout changes reach the next request.
func TestLoopToolSetStaysWithIssuingRequest(t *testing.T) {
	for _, reuseSlice := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse_slice=%t", reuseSlice), func(t *testing.T) {
			var executed []string
			makeTool := func(name string) tool.Tool {
				return tool.Tool{Name: name, Execute: func(context.Context, map[string]any) (tool.Result, error) {
					executed = append(executed, name)
					return tool.Text("from " + name), nil
				}}
			}
			selected := []tool.Tool{makeTool("old")}
			requests := 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				req := readLoopRequest(t, r)
				wantTool := "new"
				if requests == 1 {
					wantTool = "old"
				}
				if len(req.Tools) != 1 || req.Tools[0].Name != wantTool {
					t.Errorf("request %d tools = %+v, want %s", requests, req.Tools, wantTool)
				}
				switch requests {
				case 1:
					if reuseSlice {
						selected[0] = makeTool("new")
					} else {
						selected = []tool.Tool{makeTool("new")}
					}
					return phaseTestResponse(false, loopCall("first", "old", `{}`))
				case 2:
					if req.Input[len(req.Input)-1].Output != "from old" {
						t.Error("tool selection changed retroactively for the in-flight request")
					}
					return phaseTestResponse(false, loopCall("second", "new", `{}`))
				default:
					if req.Input[len(req.Input)-1].Output != "from new" {
						t.Error("new tool implementation was not used by the next request")
					}
					return phaseTestResponse(false, finalAnswerOutput)
				}
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 3, Tools: func() []tool.Tool { return selected }}}
			if err := runLoopTurn(t.Context(), a, "work"); err != nil {
				t.Fatal(err)
			}
			if requests != 3 || !slices.Equal(executed, []string{"old", "new"}) {
				t.Fatalf("requests=%d executed=%v", requests, executed)
			}
		})
	}
}

// Codex stream_error_allows_next_turn.rs: continue_after_stream_error.
// Pi agent.test.ts: full lifecycle events for failed runs. Also exercise retry
// exhaustion and a partial tool call, preserving queued input across failure.
func TestLoopProviderFailureAllowsNextTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		attempts int
	}{
		{"terminal failure", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_request_error\",\"message\":\"bad request\"}}}\n\n", 1},
		{"retry exhaustion", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"unavailable\"}}}\n\n", maxStreamRetries + 1},
		{"partial stream", "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"partial\",\"delta\":\"uncommitted\"}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + loopCall("partial", "probe", `{}`) + "}\n\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests, executions := 0, 0
				var a *Agent
				client := streamingTestClient(func(r *http.Request) string {
					requests++
					req := readLoopRequest(t, r)
					if requests <= tc.attempts {
						if requests == 1 && !a.QueueInputWithID([]Content{{Text: "queued direction"}}, "queued") {
							t.Error("queued input was rejected")
						}
						if got := loopInputOrder(req.Input); !slices.Equal(got, []string{"user:first prompt"}) {
							t.Errorf("failed request mutated its input: %v", got)
						}
						return tc.body
					}
					want := []string{"user:first prompt", "user:queued direction", "user:follow up"}
					if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
						t.Errorf("next turn input=%v, want %v", got, want)
					}
					return phaseTestResponse(false, finalAnswerOutput)
				})
				a = &Agent{Config: &Config{client: &client, Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						executions++
						return tool.Text("ran"), nil
					}}}
				}}}
				if err := runLoopTurn(t.Context(), a, "first prompt"); err == nil {
					t.Fatal("expected the first turn to fail")
				}
				if requests != tc.attempts || a.Running() || executions != 0 {
					t.Fatalf("requests=%d running=%t executions=%d", requests, a.Running(), executions)
				}
				if err := runLoopTurn(t.Context(), a, "follow up"); err != nil {
					t.Fatal(err)
				}
				var terminals []RuntimeStatus
				turnIDs := map[string]bool{}
				for _, event := range a.Events {
					if event.Type == EventTurnTerminal {
						terminals = append(terminals, event.Terminal.Status)
						turnIDs[event.TurnID] = true
					}
				}
				if requests != tc.attempts+1 || len(turnIDs) != 2 || !slices.Equal(terminals, []RuntimeStatus{RuntimeFailed, RuntimeCompleted}) {
					t.Fatalf("requests=%d terminal statuses=%v turn IDs=%v", requests, terminals, turnIDs)
				}
			})
		})
	}
}

// Codex retries preserve accepted history; Pi only dispatches accepted assistant
// tool calls. A completed call item in a failed stream is still uncommitted.
func TestLoopRetryDoesNotExecuteUncommittedCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests, executions, resets, retries := 0, 0, 0, 0
		client := streamingTestClient(func(r *http.Request) string {
			requests++
			req := readLoopRequest(t, r)
			if requests <= 2 && !slices.Equal(loopInputOrder(req.Input), []string{"user:work"}) {
				t.Error("retry included uncommitted output")
			}
			switch requests {
			case 1:
				return "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"partial\",\"delta\":\"uncommitted\"}\n\n" +
					"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + loopCall("write", "probe", `{}`) + "}\n\n" +
					"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"retry\"}}}\n\n"
			case 2:
				if executions != 0 {
					t.Error("tool ran before the successful retry")
				}
				return phaseTestResponse(false, loopCall("write", "probe", `{}`))
			default:
				want := []string{"user:work", "call:write", "result:write"}
				if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
					t.Errorf("continuation input=%v, want %v", got, want)
				}
				return phaseTestResponse(false, finalAnswerOutput)
			}
		})
		a := &Agent{Config: &Config{client: &client, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				executions++
				return tool.Text("written"), nil
			}}}
		}}}
		ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Reset: func() { resets++ }, Retry: func(RetryInfo) { retries++ }})
		if err := runLoopTurn(ctx, a, "work"); err != nil {
			t.Fatal(err)
		}
		if requests != 3 || executions != 1 || resets != 1 || retries != 1 {
			t.Fatalf("requests=%d executions=%d resets=%d retries=%d", requests, executions, resets, retries)
		}
	})
}

// Pi agent.test.ts: prompt while streaming must reject without altering the
// active transcript; callers explicitly steer via the queue instead.
func TestLoopConcurrentSendPreservesActiveTurn(t *testing.T) {
	requests := 0
	var a *Agent
	client := streamingTestClient(func(r *http.Request) string {
		requests++
		req := readLoopRequest(t, r)
		if requests == 1 {
			if _, err := a.Send(t.Context(), []Content{{Text: "rejected prompt"}}); !errors.Is(err, ErrTurnInProgress) {
				t.Errorf("concurrent Send = %v", err)
			}
			if !a.QueueInput([]Content{{Text: "accepted steering"}}) {
				t.Error("rejected Send disrupted the active turn")
			}
		} else {
			want := []string{"user:start", "assistant:Checked and fixed.", "user:accepted steering"}
			if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
				t.Errorf("continuation input=%v, want %v", got, want)
			}
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})
	a = &Agent{Config: &Config{client: &client}}
	if err := runLoopTurn(t.Context(), a, "start"); err != nil {
		t.Fatal(err)
	}
	starts, terminals := 0, 0
	for _, event := range a.Events {
		if event.Type == EventTurnStarted {
			starts++
		}
		if event.Type == EventTurnTerminal {
			terminals++
		}
	}
	if requests != 2 || starts != 1 || terminals != 1 || a.Running() {
		t.Fatalf("requests=%d turn starts=%d terminals=%d running=%t", requests, starts, terminals, a.Running())
	}
}

// Pi agent-loop.test.ts: pre-tool argument rewrites reach execution.
// Codex tool_lifecycle.rs: tool_start_receives_rewritten_payload_and_post_hook_history.
func TestLoopHookRewritesReachExecutorAndContinuation(t *testing.T) {
	const original = `{"path":"original"}`
	const updated = `{"path":"rewritten"}`
	requests := 0
	client := streamingTestClient(func(r *http.Request) string {
		requests++
		req := readLoopRequest(t, r)
		if requests == 1 {
			return phaseTestResponse(false, loopCall("edit", "probe", original))
		}
		if len(req.Input) != 3 || req.Input[1].Arguments != original {
			t.Fatalf("continuation lost original call provenance: %+v", req.Input)
		}
		output := req.Input[2].Output
		for _, want := range []string{"sanitized result", "pre guidance", "post guidance"} {
			if !strings.Contains(output, want) {
				t.Errorf("continuation omitted hook feedback %q: %q", want, output)
			}
		}
		if strings.Contains(output, "raw result") {
			t.Error("unsanitized tool output reached the continuation")
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})
	var observed []string
	a := &Agent{Config: &Config{client: &client, Hooks: hook.Hooks{
		PreToolUse: []hook.PreToolUse{func(_ context.Context, call tool.ToolCall) (hook.PreToolUseOutcome, error) {
			observed = append(observed, "pre:"+call.Args)
			return hook.PreToolUseOutcome{UpdatedInput: json.RawMessage(updated), Outcome: hook.Outcome{AdditionalContext: []string{"pre guidance"}}}, nil
		}},
		PostToolUse: []hook.PostToolUse{func(_ context.Context, call tool.ToolCall, result string) (hook.Outcome, error) {
			observed = append(observed, "post:"+call.Args+":"+result)
			replacement := "sanitized result"
			return hook.Outcome{UpdatedResult: &replacement, AdditionalContext: []string{"post guidance"}}, nil
		}},
	}, Tools: func() []tool.Tool {
		return []tool.Tool{{Name: "probe", Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			call, _ := hook.ToolCallFromContext(ctx)
			observed = append(observed, "execute:"+call.Args+":"+fmt.Sprint(args["path"]))
			return tool.Text("raw result"), nil
		}}}
	}}}
	if err := runLoopTurn(t.Context(), a, "work"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pre:" + original, "execute:" + updated + ":rewritten", "post:" + updated + ":raw result"}
	if requests != 2 || !slices.Equal(observed, want) {
		t.Fatalf("requests=%d hook/execution trace=%v, want %v", requests, observed, want)
	}
}
