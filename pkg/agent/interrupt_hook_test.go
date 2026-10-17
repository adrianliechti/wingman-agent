package agent

import (
	"context"
	"net/http"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func interruptProbeAgent(postToolUse []hook.PostToolUse, execute func(context.Context) error, fired *[]hook.Runtime) *Agent {
	client := streamingTestClient(func(*http.Request) string {
		return phaseTestResponse(false, loopCall("call-1", "probe", `{}`))
	})
	return &Agent{Config: &Config{client: &client, MaxTurns: 3, MaxParallelTools: 1,
		Hooks: hook.Hooks{
			PostToolUse: postToolUse,
			Interrupt: []hook.Interrupt{func(ctx context.Context) {
				if ctx.Err() != nil {
					panic("Interrupt hook received a canceled context")
				}
				*fired = append(*fired, hook.RuntimeFromContext(ctx))
			}},
		},
		Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(tool.EffectMutates), Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
				return tool.Result{Content: "done"}, execute(ctx)
			}}}
		},
	}}
}

func TestInterruptHookFiresWhenUserCancelsTurn(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	var fired []hook.Runtime
	a := interruptProbeAgent(nil, func(toolCtx context.Context) error {
		cancel(ErrInterrupted)
		<-toolCtx.Done()
		return toolCtx.Err()
	}, &fired)
	_ = runLoopTurn(ctx, a, "work")
	if len(fired) != 1 {
		t.Fatalf("Interrupt fired %d times, want 1", len(fired))
	}
	var turnID string
	for _, event := range a.Events {
		if event.Type == EventTurnStarted {
			turnID = event.TurnID
		}
	}
	if fired[0].TurnID == "" || fired[0].TurnID != turnID {
		t.Fatalf("Interrupt runtime turn = %q, want %q", fired[0].TurnID, turnID)
	}
}

func TestInterruptHookIgnoresHookStops(t *testing.T) {
	var fired []hook.Runtime
	stop := []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
		return hook.Outcome{Stop: true, Reason: "policy stop"}, nil
	}}
	a := interruptProbeAgent(stop, func(context.Context) error { return nil }, &fired)
	if err := runLoopTurn(t.Context(), a, "work"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 0 {
		t.Fatalf("hook stop fired Interrupt %d times", len(fired))
	}
}

func TestInterruptHookIgnoresTaskBudgets(t *testing.T) {
	var fired []hook.Runtime
	a := interruptProbeAgent(nil, func(ctx context.Context) error {
		chargeTaskUsage(ctx, Usage{InputTokens: 10})
		<-ctx.Done()
		return ctx.Err()
	}, &fired)
	a.MaxTaskTokens = 1
	_ = runLoopTurn(t.Context(), a, "work")
	if len(fired) != 0 {
		t.Fatalf("task budget fired Interrupt %d times", len(fired))
	}
}

func TestInterruptHookIgnoresOtherCancellations(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var fired []hook.Runtime
	a := interruptProbeAgent(nil, func(toolCtx context.Context) error {
		cancel()
		<-toolCtx.Done()
		return toolCtx.Err()
	}, &fired)
	_ = runLoopTurn(ctx, a, "work")
	if len(fired) != 0 {
		t.Fatalf("closing the session fired Interrupt %d times", len(fired))
	}
}
