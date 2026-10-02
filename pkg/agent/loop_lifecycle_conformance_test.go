package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Pi agent-loop.test.ts: runToolCall failure outcomes; agent.test.ts: lifecycle
// events on failure. Wingman runs post hooks for blocked/invalid calls too, so
// assert its public result and ledger contracts rather than Pi's hook ordering.
func TestLoopToolFailuresStillCompleteTheBatch(t *testing.T) {
	for _, tc := range []struct {
		name, toolName, args, message string
		wantExecutions                int
		wantMetadata                  bool
	}{
		{"unknown", "missing", `{}`, "unknown tool", 1, false},
		{"malformed arguments", "probe", `{`, "failed to parse arguments", 1, false},
		{"non-object arguments", "probe", `[]`, "failed to parse arguments", 1, false},
		{"executor error", "probe", `{}`, "executor failed", 2, true},
		{"reported error", "probe", `{}`, "reported failure", 2, true},
		{"executor panic", "probe", `{}`, "panicked", 2, false},
		{"pre block", "probe", `{}`, "blocked by policy", 1, false},
		{"pre error", "probe", `{}`, "pre hook failed", 1, false},
		{"post error", "probe", `{}`, "post hook failed", 2, true},
		{"post panic", "probe", `{}`, "panicked", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, executions := 0, 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				req := readLoopRequest(t, r)
				if requests == 1 {
					return phaseTestResponse(false, loopCall("bad", tc.toolName, tc.args), loopCall("good", "probe", `{}`))
				}
				want := []string{"user:work", "call:bad", "call:good", "result:bad", "result:good"}
				if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
					t.Errorf("continuation input=%v, want %v", got, want)
				} else if !strings.Contains(req.Input[3].Output, tc.message) || req.Input[4].Output != "sibling succeeded" {
					t.Errorf("failure or sibling result lost: %+v", req.Input[3:])
				}
				return phaseTestResponse(false, finalAnswerOutput)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2, Hooks: hook.Hooks{
				PreToolUse: []hook.PreToolUse{func(_ context.Context, call tool.ToolCall) (hook.PreToolUseOutcome, error) {
					if call.ID == "bad" {
						switch tc.name {
						case "pre block":
							return hook.PreToolUseOutcome{Outcome: hook.Outcome{Block: true, Reason: "blocked by policy"}}, nil
						case "pre error":
							return hook.PreToolUseOutcome{}, errors.New("pre hook failed")
						}
					}
					return hook.PreToolUseOutcome{}, nil
				}},
				PostToolUse: []hook.PostToolUse{func(_ context.Context, call tool.ToolCall, _ string) (hook.Outcome, error) {
					if call.ID == "bad" {
						switch tc.name {
						case "post error":
							return hook.Outcome{}, errors.New("post hook failed")
						case "post panic":
							panic("post hook failed")
						}
					}
					return hook.Outcome{}, nil
				}},
			}, Tools: func() []tool.Tool {
				return []tool.Tool{{Name: "probe", Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
					executions++
					call, _ := hook.ToolCallFromContext(ctx)
					if call.ID == "good" {
						return tool.Text("sibling succeeded"), nil
					}
					result := tool.Result{Content: "raw output", Metadata: map[string]any{"observed": true}}
					switch tc.name {
					case "executor error":
						return result, errors.New("executor failed")
					case "reported error":
						result.Content, result.IsError = "reported failure", true
					case "executor panic":
						panic("executor failed")
					}
					return result, nil
				}}}
			}}}
			if err := runLoopTurn(t.Context(), a, "work"); err != nil {
				t.Fatal(err)
			}
			starts, terminals := map[string]bool{}, map[string]bool{}
			var bad *ToolResult
			for _, event := range a.Events {
				if event.Type == EventToolStarted {
					starts[event.OperationID] = true
				}
				if event.Type == EventToolTerminal {
					if !starts[event.OperationID] || terminals[event.OperationID] {
						t.Error("tool terminal is unmatched or duplicated")
					}
					terminals[event.OperationID] = true
					want := RuntimeCompleted
					if event.Tool.CallID == "bad" {
						want = RuntimeFailed
					}
					if event.Terminal.Status != want {
						t.Errorf("tool %s status=%s, want %s", event.Tool.CallID, event.Terminal.Status, want)
					}
				}
				if event.Message != nil {
					for _, c := range event.Message.Content {
						if c.ToolResult != nil && c.ToolResult.ID == "bad" {
							bad = c.ToolResult
						}
					}
				}
			}
			if requests != 2 || executions != tc.wantExecutions || len(starts) != 2 || len(terminals) != 2 {
				t.Errorf("requests=%d executions=%d starts=%d terminals=%d", requests, executions, len(starts), len(terminals))
			}
			if bad == nil || !bad.IsError || !strings.Contains(bad.Content, tc.message) {
				t.Fatalf("bad call result=%+v", bad)
			}
			if tc.wantMetadata && bad.Metadata["observed"] != true {
				t.Error("failure discarded completed work metadata")
			}
		})
	}
}

// Pi agent.test.ts: ignore updates from a settled tool, including while a
// parallel sibling is still running. Exercise both default and disabled timeouts.
func TestLoopProgressStopsWhenItsToolReturns(t *testing.T) {
	for _, timeout := range []time.Duration{0, -1} {
		t.Run(fmt.Sprintf("timeout=%s", timeout), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				client := streamingTestClient(func(*http.Request) string {
					requests++
					if requests == 1 {
						return phaseTestResponse(false, loopCall("fast", "probe", `{"wait":false}`), loopCall("slow", "probe", `{"wait":true}`))
					}
					return phaseTestResponse(false, finalAnswerOutput)
				})
				var fastProgress, slowProgress func(string)
				release := make(chan struct{})
				a := &Agent{Config: &Config{client: &client, ToolTimeout: timeout, Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
						progress := tool.Progress(ctx)
						if args["wait"] == true {
							slowProgress = progress
							<-release
						} else {
							fastProgress = progress
							progress("running")
						}
						return tool.Text("done"), nil
					}}}
				}}}
				var mu sync.Mutex
				var updates []string
				ctx := tool.WithProgressSink(t.Context(), func(_ context.Context, id, text string) {
					mu.Lock()
					defer mu.Unlock()
					updates = append(updates, id+":"+text)
				})
				done := make(chan error, 1)
				go func() { done <- runLoopTurn(ctx, a, "work") }()
				synctest.Wait() // Fast is settled; slow and the agent are blocked on release.
				if fastProgress == nil || slowProgress == nil {
					t.Error("tools did not receive progress reporters")
				} else {
					fastProgress("late during sibling")
					slowProgress("still running")
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if fastProgress != nil {
					fastProgress("late after turn")
				}
				mu.Lock()
				defer mu.Unlock()
				want := []string{"fast:running", "slow:still running"}
				if requests != 2 || !slices.Equal(updates, want) {
					t.Fatalf("requests=%d progress=%v, want %v", requests, updates, want)
				}
			})
		})
	}
}

// Codex abort_lifecycle.rs: cleanup finishes before the terminal event.
// Adapt the abort callback gate to Wingman's in-flight tool executors.
func TestLoopCancellationDrainsToolsBeforeTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		client := streamingTestClient(func(*http.Request) string {
			requests++
			if requests == 1 {
				return phaseTestResponse(false, loopCall("first", "probe", `{}`), loopCall("second", "probe", `{}`))
			}
			return phaseTestResponse(false, finalAnswerOutput)
		})
		started := make(chan struct{}, 2)
		var cleaned atomic.Int64
		a := &Agent{Config: &Config{client: &client, ToolTimeout: -1, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
				started <- struct{}{}
				<-ctx.Done()
				time.Sleep(time.Second) // Simulate asynchronous cleanup in virtual time.
				cleaned.Add(1)
				return tool.Result{}, ctx.Err()
			}}}
		}}}
		toolTerminals, turnTerminals := 0, 0
		a.Recorder = EventRecorderFunc(func(events []RuntimeEvent) error {
			for _, event := range events {
				if event.Type == EventToolTerminal {
					toolTerminals++
					if event.Terminal.Status != RuntimeInterrupted {
						t.Error("canceled tool did not record an interrupted outcome")
					}
				}
				if event.Type == EventTurnTerminal {
					turnTerminals++
					if cleaned.Load() != 2 || toolTerminals != 2 {
						t.Error("turn ended before tool cleanup and terminal facts")
					}
				}
			}
			return nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runLoopTurn(ctx, a, "work") }()
		<-started
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled turn error=%v", err)
		}
		if a.Running() || requests != 1 || turnTerminals != 1 {
			t.Fatalf("running=%t requests=%d turn terminals=%d", a.Running(), requests, turnTerminals)
		}
		if err := runLoopTurn(t.Context(), a, "continue"); err != nil {
			t.Fatal(err)
		}
		if requests != 2 || toolTerminals != 2 || turnTerminals != 2 {
			t.Fatalf("resume replayed tools or left duplicate terminals: requests=%d tool terminals=%d turn terminals=%d", requests, toolTerminals, turnTerminals)
		}
	})
}
