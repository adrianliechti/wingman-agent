package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

func TestLifecycleHookErrorsAreReportedWithoutChangingBestEffortPolicy(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	failure := errors.New("hook unavailable")
	var continued int
	fail := func(context.Context, string) (hook.Outcome, error) { return hook.Outcome{}, failure }
	continueHook := func(context.Context, string) (hook.Outcome, error) { continued++; return hook.Outcome{}, nil }
	a := &Agent{Config: &Config{Hooks: hook.Hooks{
		SessionStart: []hook.SessionStart{fail, continueHook},
		Stop: []hook.Stop{
			func(context.Context, string, bool) (hook.Outcome, error) { return hook.Outcome{}, failure },
			func(context.Context, string, bool) (hook.Outcome, error) { continued++; return hook.Outcome{}, nil },
		},
		SubagentStop: []hook.SubagentStop{
			func(context.Context, string, string, string, bool) (hook.Outcome, error) {
				return hook.Outcome{}, failure
			},
			func(context.Context, string, string, string, bool) (hook.Outcome, error) {
				continued++
				return hook.Outcome{}, nil
			},
		},
	}}}
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "session", TurnID: "turn", AgentID: "child"})
	if out, err := a.runSessionStartHooks(ctx, "startup"); err != nil || out.Stop || out.Block {
		t.Fatalf("startup policy changed: %+v, %v", out, err)
	}
	if out := a.runStopHooks(ctx, "done", false); out.Stop || out.Block {
		t.Fatalf("stop policy changed: %+v", out)
	}
	for _, event := range []string{"PreCompact", "PostCompact"} {
		if out := runCompactHooks(ctx, event, "manual", []hook.PreCompact{fail, continueHook}); out.Stop || out.Block {
			t.Fatalf("%s policy changed: %+v", event, out)
		}
	}
	if continued != 5 {
		t.Fatalf("only %d later hooks ran", continued)
	}
	decoder := json.NewDecoder(&logs)
	for _, event := range []string{"SessionStart", "Stop", "SubagentStop", "PreCompact", "PostCompact"} {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["event"] != event || entry["turn_id"] != "turn" || entry["error"] != failure.Error() {
			t.Fatalf("unexpected diagnostic: %v", entry)
		}
	}
}
