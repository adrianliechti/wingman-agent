package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
)

func reviewConfig(t *testing.T, output func() []any) *harness.Config {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": output()}})
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	t.Cleanup(server.Close)
	t.Setenv("WINGMAN_URL", server.URL)
	cfg, err := harness.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func reviewText(text string) []any {
	return []any{map[string]any{"type": "message", "id": "msg", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}
}

func reviewEvent(t *testing.T, registry *task.Registry) task.Event {
	t.Helper()
	select {
	case ev := <-registry.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
		return task.Event{}
	}
}

func TestReviewAgentPartialFailureRemainsFailure(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint(background), func(t *testing.T) {
			cfg := reviewConfig(t, func() []any { return reviewText("partial report") })
			cfg.MaxTurns = 1
			cfg.Hooks.SubagentStop = []hook.SubagentStop{func(context.Context, string, string, string, bool) (hook.Outcome, error) {
				return hook.Outcome{Block: true, Reason: "more work required"}, nil
			}}
			registry := task.NewRegistry()
			defer registry.Close()
			out, err := Tools(cfg, nil, registry)[0].Execute(t.Context(), map[string]any{"description": "work", "prompt": "work", "agent_type": "explore", "background": background})
			if background {
				if err != nil {
					t.Fatal(err)
				}
				ev := reviewEvent(t, registry)
				if ev.Status != task.StatusFailed || !strings.Contains(ev.Result, "partial report") || !strings.Contains(ev.Result, "incomplete") {
					t.Fatalf("failure reported as completion: %+v", ev)
				}
			} else {
				if err == nil && !out.IsError {
					t.Fatalf("partial failure returned success: %#v", out)
				}
				if !strings.Contains(out.Content, "partial report") || !strings.Contains(out.Content, registry.List()[0].ID) {
					t.Fatalf("failure lost output or follow-up id: %#v, %v", out, err)
				}
			}
			if registry.List()[0].Status() != task.StatusFailed {
				t.Fatalf("task status=%s", registry.List()[0].Status())
			}
		})
	}
}

func TestReviewAgentSchemaRequiresCurrentReport(t *testing.T) {
	var calls atomic.Int32
	cfg := reviewConfig(t, func() []any {
		if calls.Add(1) == 1 {
			return []any{map[string]any{"type": "function_call", "id": "fc_report", "call_id": "report-1", "name": "report", "arguments": `{"result":{"count":1}}`, "status": "completed"}}
		}
		return reviewText("No new structured report")
	})
	cfg.MaxTurns = 1
	spec := durableSpec{Version: durableSpecVersion, AgentID: "child", AgentType: "explore", Schema: map[string]any{"type": "object"}}
	runner, err := newSubagentRunner(cfg, subagentTypes["explore"], spec, harness.State{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.runTurn(t.Context(), t.Context(), nil, "first"); err == nil {
		t.Fatal("first run must fail at the turn limit")
	}
	out, err := runner.runTurn(t.Context(), t.Context(), nil, "second")
	if err == nil || strings.Contains(out, `{"count":1}`) {
		t.Fatalf("follow-up accepted a stale report: %q, %v", out, err)
	}
}

func TestReviewAgentMissingReportFails(t *testing.T) {
	cfg := reviewConfig(t, func() []any { return reviewText("unstructured answer") })
	out, err := Tools(cfg, nil, nil)[0].Execute(t.Context(), map[string]any{"description": "work", "prompt": "work", "agent_type": "explore", "schema": map[string]any{"type": "object"}})
	if err == nil && !out.IsError {
		t.Fatalf("missing report accepted: %#v", out)
	}
}

func TestReviewAgentHookStopIsNotSuccess(t *testing.T) {
	cfg := reviewConfig(t, func() []any { return reviewText("unfinished answer") })
	cfg.Hooks.SubagentStop = []hook.SubagentStop{func(context.Context, string, string, string, bool) (hook.Outcome, error) {
		return hook.Outcome{Stop: true, Reason: "stop requested by hook"}, nil
	}}
	out, err := Tools(cfg, nil, nil)[0].Execute(t.Context(), map[string]any{"description": "work", "prompt": "work", "agent_type": "explore"})
	if err == nil && !out.IsError {
		t.Fatalf("interrupted child reported success: %#v", out)
	}
}

func TestReviewAgentInvalidOptionsSkipStartHooks(t *testing.T) {
	for key, values := range map[string][]any{
		"background": {"true", nil, 1},
		"model":      {17, nil, "", "unknown"},
		"effort":     {false, nil, "", "unknown"},
		"schema":     {nil, []any{}, map[string]any{"type": 17}},
	} {
		for _, value := range values {
			t.Run(fmt.Sprintf("%s/%v", key, value), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				cfg := &harness.Config{Hooks: hook.Hooks{SubagentStart: []hook.SubagentStart{func(context.Context, string, string) (hook.Outcome, error) {
					calls++
					cancel()
					return hook.Outcome{}, nil
				}}}}
				args := map[string]any{"description": "work", "prompt": "work", "agent_type": "explore", key: value}
				_, err := Tools(cfg, nil, nil)[0].Execute(ctx, args)
				if err == nil || calls != 0 {
					t.Fatalf("invalid option ran hooks: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestReviewCanceledAgentControls(t *testing.T) {
	registry := task.NewRegistry()
	defer registry.Close()
	release := make(chan struct{})
	defer close(release)
	var stopped atomic.Bool
	tk, err := registry.Launch("work", "explore", func(ctx context.Context, _ *task.Task) (string, error) {
		<-ctx.Done()
		stopped.Store(true)
		<-release
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tl := range taskTools(registry) {
		args := map[string]any{"id": tk.ID, "message": "continue"}
		if _, err := tl.Execute(ctx, args); !errors.Is(err, context.Canceled) {
			t.Errorf("%s ignored cancellation: %v", tl.Name, err)
		}
	}
	if stopped.Load() {
		t.Fatal("canceled stop request stopped an agent")
	}
}

func TestReviewAgentOutputValidatesOptionalID(t *testing.T) {
	registry := task.NewRegistry()
	defer registry.Close()
	for _, id := range []any{nil, 1, false, "  "} {
		if _, err := taskTools(registry)[0].Execute(t.Context(), map[string]any{"id": id}); err == nil {
			t.Fatalf("invalid id %v listed all tasks", id)
		}
	}
}

func TestReviewReportCannotBeOverwrittenOrRecordedAfterCancellation(t *testing.T) {
	collector, err := newReportCollector(map[string]any{"type": "string"})
	if err != nil {
		t.Fatal(err)
	}
	report := collector.tool()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := report.Execute(ctx, map[string]any{"result": "canceled"}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled report accepted: %v", err)
	}
	if got := collector.take(); got != "" {
		t.Errorf("canceled result recorded: %s", got)
	}
	if _, err := report.Execute(t.Context(), map[string]any{"result": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := report.Execute(t.Context(), map[string]any{"result": "second"}); err == nil {
		t.Error("duplicate report replaced the first result")
	}
	if got := collector.take(); got != `"first"` {
		t.Fatalf("result overwritten: %s", got)
	}
}

func TestReviewFinalTextExcludesHiddenAssistantText(t *testing.T) {
	if got := finalText([]harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "report"}}}, {Role: harness.RoleAssistant, Hidden: true, Content: []harness.Content{{Text: "internal"}}}}); got != "report" {
		t.Fatalf("report included hidden text: %q", got)
	}
}

func TestReviewInvalidEffortDoesNotInvokeRoleModel(t *testing.T) {
	calls := 0
	cfg := &harness.Config{
		RoleModel: func(string) (harness.ModelOption, bool) {
			calls++
			return harness.ModelOption{ID: "utility-model"}, true
		},
	}
	_, err := Tools(cfg, nil, nil)[0].Execute(t.Context(), map[string]any{
		"description": "work",
		"prompt":      "work",
		"agent_type":  "explore",
		"model":       "utility",
		"effort":      "invalid-effort",
	})
	if err == nil || calls != 0 {
		t.Fatalf("invalid effort invoked RoleModel: calls=%d err=%v", calls, err)
	}
}

func TestReviewToolsDoesNotRebindActiveTasks(t *testing.T) {
	registry := task.NewRegistry()
	defer registry.Close()
	spec := durableSpec{
		Version: durableSpecVersion, AgentID: "live-agent", AgentType: "explore",
		Instructions: "instructions",
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	tk, err := registry.LaunchAgent(spec.AgentID, "work", "explore", func(tk *task.Task) error {
		return tk.SetDurableAgent(spec.AgentID, harness.State{}, raw)
	}, func(context.Context, *task.Task) (string, error) {
		close(started)
		<-release
		return "done", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	resumed := false
	tk.SetResume(func(string) error {
		resumed = true
		return nil
	})
	_ = Tools(nil, nil, registry)
	if !tk.SupportsResume() {
		t.Fatal("Tools cleared existing resume binding")
	}
	if resumed {
		t.Fatal("unexpected resume call")
	}
}
