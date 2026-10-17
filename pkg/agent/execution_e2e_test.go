package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
)

func TestExecutionSettingsE2EStayWithIssuingRequest(t *testing.T) {
	type settings struct{ model, effort, mode string }
	var selected atomic.Value
	selected.Store(settings{"model-a", "high", "default"})
	var a *agent.Agent
	var rounds atomic.Int64
	provider := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
		n := rounds.Add(1)
		if n == 1 {
			// Settings change before the response's tool call is received.
			selected.Store(settings{"model-b", "low", "plan"})
			if !a.QueueInput([]agent.Content{{Text: "continue with the updated settings"}}) {
				t.Error("queued input was rejected")
			}
		}
		if n <= 2 {
			fmt.Fprint(w, completedToolCall(fmt.Sprintf("call-%d", n), "inspect"))
		} else {
			fmt.Fprint(w, completedText("done"))
		}
	})
	type hookPayload struct {
		Event  string `json:"hook_event_name"`
		Model  string `json:"model"`
		Effort string `json:"reasoning_effort"`
		Mode   string `json:"permission_mode"`
		Turn   string `json:"turn_id"`
	}
	var mu sync.Mutex
	var payloads []hookPayload
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload hookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(hookServer.Close)
	groups := []external.MatcherGroup{{Hooks: []external.Handler{{Type: "http", URL: hookServer.URL}}}}
	hooks := (&external.Config{Hooks: external.Events{
		PreToolUse: groups, PostToolUse: groups, PermissionRequest: groups, Stop: groups, UserPromptSubmit: groups,
	}}).Build(t.TempDir(), nil)
	cfg := e2eConfig(t, tool.Tool{Name: "inspect", Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
		call, ok := hook.ToolCallFromContext(ctx)
		if !ok {
			t.Error("tool lost its call identity")
		}
		_, err := hooks.PermissionRequest[0](ctx, call)
		return tool.Text("observed"), err
	}})
	cfg.Model = func() string { return selected.Load().(settings).model }
	cfg.Effort = func() string { return selected.Load().(settings).effort }
	cfg.PermissionMode = func() string { return selected.Load().(settings).mode }
	cfg.Hooks = hooks
	a = &agent.Agent{Config: cfg}
	if err := drain(t, a, "inspect twice"); err != nil {
		t.Fatal(err)
	}
	requests := provider.snapshot()
	if len(requests) != 3 || requests[0].Model != "model-a" || requests[1].Model != "model-b" {
		t.Fatalf("requests = %+v", requests)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 9 {
		t.Fatalf("hook payloads = %+v", payloads)
	}
	turn := payloads[0].Turn
	if turn == "" {
		t.Fatal("missing turn identity")
	}
	want := []hookPayload{
		{"UserPromptSubmit", "model-a", "high", "default", turn},
		{"PreToolUse", "model-a", "high", "default", turn},
		{"PermissionRequest", "model-a", "high", "default", turn},
		{"PostToolUse", "model-a", "high", "default", turn},
		{"UserPromptSubmit", "model-b", "low", "plan", turn},
		{"PreToolUse", "model-b", "low", "plan", turn},
		{"PermissionRequest", "model-b", "low", "plan", turn},
		{"PostToolUse", "model-b", "low", "plan", turn},
		{"Stop", "model-b", "low", "plan", turn},
	}
	if !reflect.DeepEqual(payloads, want) {
		t.Fatalf("hook payloads = %+v, want %+v", payloads, want)
	}
}

func TestExecutionSettingsE2ESubagentUsesItsOwnModel(t *testing.T) {
	var mode atomic.Value
	mode.Store("plan")
	var rounds atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if req.Model != "child-model" {
			t.Errorf("child requested model %q", req.Model)
		}
		switch rounds.Add(1) {
		case 1:
			mode.Store("default")
			fmt.Fprint(w, completedToolCall("child-call", "inspect"))
		case 2:
			mode.Store("bypassPermissions")
			fmt.Fprint(w, completedText("child report"))
		default:
			fmt.Fprint(w, completedText("child report"))
		}
	})
	var observed []hook.Runtime
	record := func(ctx context.Context) { observed = append(observed, hook.RuntimeFromContext(ctx)) }
	cfg := e2eConfig(t, tool.Tool{Name: "inspect", Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
		record(ctx)
		return tool.Text("checked"), nil
	}})
	cfg.RoleModel = func(string) (agent.ModelOption, bool) { return agent.ModelOption{ID: "child-model"}, true }
	cfg.PermissionMode = func() string { return mode.Load().(string) }
	cfg.Hooks.PreToolUse = []hook.PreToolUse{func(ctx context.Context, _ tool.ToolCall) (hook.PreToolUseOutcome, error) {
		record(ctx)
		return hook.PreToolUseOutcome{}, nil
	}}
	cfg.Hooks.PostToolUse = []hook.PostToolUse{func(ctx context.Context, _ tool.ToolCall, _ string) (hook.Outcome, error) {
		record(ctx)
		return hook.Outcome{}, nil
	}}
	stops := 0
	cfg.Hooks.SubagentStop = []hook.SubagentStop{func(ctx context.Context, _, _, _ string, active bool) (hook.Outcome, error) {
		record(ctx)
		stops++
		if active != (stops > 1) {
			t.Errorf("SubagentStop active=%t on call %d", active, stops)
		}
		if stops == 1 {
			return hook.Outcome{Block: true, Reason: "verify the child report"}, nil
		}
		return hook.Outcome{}, nil
	}}
	parent := hook.Runtime{SessionID: "parent-session", TurnID: "parent-turn", Model: "parent-model", ReasoningEffort: "high", PermissionMode: "default"}
	ctx := hook.WithRuntime(t.Context(), parent)
	result, err := subagent.Tools(cfg, nil, nil)[0].Execute(ctx, map[string]any{
		"description": "inspect child metadata", "prompt": "inspect", "agent_type": "general-purpose", "model": "plan", "effort": "low",
	})
	if err != nil || result.IsError {
		t.Fatalf("child result=%+v error=%v", result, err)
	}
	if len(observed) != 5 || len(p.snapshot()) != 3 {
		t.Fatalf("observed=%+v requests=%d", observed, len(p.snapshot()))
	}
	wantModes := []string{"plan", "plan", "plan", "default", "bypassPermissions"}
	for i, runtime := range observed {
		if runtime.Model != "child-model" || runtime.ReasoningEffort != "low" || runtime.PermissionMode != wantModes[i] || runtime.SessionID != parent.SessionID || runtime.AgentID == "" || runtime.AgentType != "general-purpose" {
			t.Errorf("child hook runtime = %+v", runtime)
		}
		if runtime.TurnID == "" || runtime.TurnID == parent.TurnID || runtime.TurnID != observed[0].TurnID {
			t.Errorf("child hooks lost their turn identity: %+v", runtime)
		}
	}
	if hook.RuntimeFromContext(ctx) != parent {
		t.Fatal("child mutated its parent's execution metadata")
	}
}
