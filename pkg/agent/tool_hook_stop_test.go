package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestPostToolStopEndsTurnAndAllowsNextTurn(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		for _, effect := range []tool.Effect{tool.EffectReadOnly, tool.EffectMutates} {
			t.Run(fmt.Sprintf("streamed=%t/%s", streamed, effect), func(t *testing.T) {
				requests, resumed := 0, false
				var executions atomic.Int32
				client := streamingTestClient(func(*http.Request) string {
					requests++
					if resumed || requests > 2 {
						return phaseTestResponse(false, finalAnswerOutput)
					}
					return phaseTestResponse(streamed, loopCall(fmt.Sprint(requests)+"-first", "probe", `{}`), loopCall(fmt.Sprint(requests)+"-second", "probe", `{}`))
				})
				a := &Agent{Config: &Config{client: &client, MaxTurns: 3, MaxParallelTools: 1,
					Hooks: hook.Hooks{PostToolUse: []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
						return hook.Outcome{Stop: true, Reason: "stop requested"}, nil
					}}},
					Tools: func() []tool.Tool {
						return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(effect), Execute: func(context.Context, map[string]any) (tool.Result, error) {
							executions.Add(1)
							return tool.Result{Content: "executed", Metadata: map[string]any{"completed": true}}, nil
						}}}
					},
				}}
				if err := runLoopTurn(t.Context(), a, "work"); err != nil {
					t.Fatal(err)
				}
				if requests != 1 || executions.Load() != 1 {
					t.Fatalf("continued after stop: requests=%d executions=%d", requests, executions.Load())
				}
				starts, terminals, results := 0, 0, 0
				for _, event := range a.Events {
					switch event.Type {
					case EventToolStarted:
						starts++
					case EventToolTerminal:
						terminals++
					case EventTurnTerminal:
						if event.Terminal.Status != RuntimeInterrupted || event.Terminal.Error != "stop requested" {
							t.Fatalf("turn terminal: %+v", event.Terminal)
						}
					}
				}
				for _, message := range a.Messages {
					for _, content := range message.Content {
						if result := content.ToolResult; result != nil {
							results++
							if result.ID == "1-first" && (result.Metadata["completed"] != true || result.Content != "stop requested") {
								t.Fatalf("completed tool result lost: %+v", result)
							}
						}
					}
				}
				// Cancellation may prevent the second streamed call from being
				// dispatched, but every started call must have a durable result.
				if starts < 1 || starts > 2 || terminals != starts || results != starts {
					t.Fatalf("incomplete tool history: starts=%d terminals=%d results=%d", starts, terminals, results)
				}
				resumed = true
				if err := runLoopTurn(t.Context(), a, "continue"); err != nil || requests != 2 {
					t.Fatalf("next turn: requests=%d error=%v", requests, err)
				}
			})
		}
	}
}

func TestPostToolStopCancelsOpenModelStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		client := heldResponseClient(itemDoneEvents(loopCall("first", "probe", `{}`)), "", make(chan struct{}), func(*http.Request) string {
			t.Error("hook stop retried the canceled stream")
			return phaseTestResponse(false, finalAnswerOutput)
		})
		a := &Agent{Config: &Config{client: &client, Hooks: hook.Hooks{PostToolUse: []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
			return hook.Outcome{Stop: true, Reason: "stop open stream"}, nil
		}}}, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				return tool.Text("executed"), nil
			}}}
		}}}
		if err := runLoopTurn(ctx, a, "work"); err != nil || ctx.Err() != nil {
			t.Fatalf("hook did not stop the pending response: error=%v context=%v", err, ctx.Err())
		}
		terminal := a.Events[len(a.Events)-1]
		if terminal.Type != EventTurnTerminal || terminal.Terminal.Status != RuntimeInterrupted || terminal.Terminal.Error != "stop open stream" {
			t.Fatalf("incorrect hook stop terminal: type=%s terminal=%+v", terminal.Type, terminal.Terminal)
		}
	})
}

func TestPostToolStopCancelsRunningAndQueuedCalls(t *testing.T) {
	secondStarted := make(chan struct{})
	var executions atomic.Int32
	a := &Agent{Config: &Config{MaxParallelTools: 2, Hooks: hook.Hooks{PostToolUse: []hook.PostToolUse{
		func(_ context.Context, call tool.ToolCall, _ string) (hook.Outcome, error) {
			if call.ID == "first" {
				<-secondStarted
				return hook.Outcome{Stop: true, Reason: "stop concurrent work"}, nil
			}
			return hook.Outcome{}, nil
		},
	}}}}
	tools := []tool.Tool{{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
		executions.Add(1)
		call, _ := hook.ToolCallFromContext(ctx)
		if call.ID == "second" {
			close(secondStarted)
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		}
		return tool.Text("read"), nil
	}}, {Name: "write", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		t.Error("mutation executed after stop")
		return tool.Text("written"), nil
	}}}
	err := a.processToolCalls(t.Context(), []ToolCall{{ID: "first", Name: "read"}, {ID: "second", Name: "read"}, {ID: "queued", Name: "read"}, {ID: "mutation", Name: "write"}}, tools, yieldAll)
	if stop, ok := errors.AsType[hookStopError](err); !ok || stop.Error() != "stop concurrent work" || executions.Load() != 2 {
		t.Fatalf("stop=%v executions=%d", err, executions.Load())
	}
}

func TestPostToolStopEndsSequentialBatch(t *testing.T) {
	executions := 0
	a := &Agent{Config: &Config{Hooks: hook.Hooks{PostToolUse: []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
		return hook.Outcome{Stop: true, Reason: "stop batch"}, nil
	}}}}}
	tools := []tool.Tool{{Name: "write", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		executions++
		return tool.Text("written"), nil
	}}}
	err := a.processToolCalls(t.Context(), []ToolCall{{ID: "first", Name: "write"}, {ID: "second", Name: "write"}}, tools, yieldAll)
	if _, ok := errors.AsType[hookStopError](err); !ok || executions != 1 {
		t.Fatalf("stop=%v executions=%d", err, executions)
	}
}
