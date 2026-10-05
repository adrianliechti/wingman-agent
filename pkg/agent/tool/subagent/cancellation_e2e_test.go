package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/task"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
)

func TestCancelledSubagentSpawnSkipsHooksAndAdmission(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			registry := task.NewRegistry()
			defer registry.Close()
			if shutdown {
				registry.Close()
			}
			cfg := &agent.Config{}
			hookCalls := 0
			cfg.Hooks.SubagentStart = []hook.SubagentStart{func(context.Context, string, string) (hook.Outcome, error) {
				hookCalls++
				return hook.Outcome{}, nil
			}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := subagent.Tools(cfg, nil, registry)[0].Execute(ctx, map[string]any{
				"description": "work", "prompt": "work", "agent_type": "explore", "background": true,
			})
			if !errors.Is(err, context.Canceled) || hookCalls != 0 || len(registry.List()) != 0 {
				t.Fatalf("error=%v hooks=%d tasks=%d", err, hookCalls, len(registry.List()))
			}
		})
	}
}

func TestParentCancellationDuringSubagentPreparationE2E(t *testing.T) {
	for _, phase := range []string{"start hook", "model resolution"} {
		t.Run(phase, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				args, _ := json.Marshal(map[string]any{
					"description": "child", "prompt": "work", "agent_type": "explore", "background": true, "model": "utility",
				})
				event := map[string]any{"type": "response.completed", "response": map[string]any{
					"output": []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "child_call", "name": "agent", "arguments": string(args), "status": "completed"}},
				}}
				encoded, _ := json.Marshal(event)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", encoded)
			}))
			defer server.Close()
			t.Setenv("WINGMAN_URL", server.URL)
			cfg, err := agent.DefaultConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxTurns = 2
			registry := task.NewRegistry()
			defer registry.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			preparations := 0
			if phase == "start hook" {
				cfg.Hooks.SubagentStart = []hook.SubagentStart{func(context.Context, string, string) (hook.Outcome, error) {
					preparations++
					cancel()
					return hook.Outcome{}, nil
				}, func(context.Context, string, string) (hook.Outcome, error) {
					t.Error("a later start hook ran after cancellation")
					return hook.Outcome{}, nil
				}}
			} else {
				cfg.RoleModel = func(string) (agent.ModelOption, bool) {
					preparations++
					cancel()
					return agent.ModelOption{ID: "child-model"}, true
				}
			}
			cfg.Tools = func() []tool.Tool { return subagent.Tools(cfg, nil, registry) }
			parent := &agent.Agent{Config: cfg}
			stream, err := parent.Send(ctx, []agent.Content{{Text: "delegate work"}})
			if err != nil {
				t.Fatal(err)
			}
			var runErr error
			for _, err := range stream {
				runErr = err
			}
			if !errors.Is(runErr, context.Canceled) || preparations != 1 || requests.Load() != 1 || len(registry.List()) != 0 {
				t.Fatalf("error=%v preparations=%d requests=%d tasks=%d", runErr, preparations, requests.Load(), len(registry.List()))
			}
		})
	}
}

func TestCancelledSubagentFollowUpDoesNotRestartE2E(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}]}}\n\n")
			}))
			defer server.Close()
			t.Setenv("WINGMAN_URL", server.URL)
			cfg, err := agent.DefaultConfig()
			if err != nil {
				t.Fatal(err)
			}
			registry := task.NewRegistry()
			defer registry.Close()
			tools := subagent.Tools(cfg, nil, registry)
			if _, err := tools[0].Execute(t.Context(), map[string]any{
				"description": "child", "prompt": "work", "agent_type": "explore",
			}); err != nil {
				t.Fatal(err)
			}
			children := registry.List()
			if len(children) != 1 || requests.Load() != 1 {
				t.Fatalf("tasks=%d requests=%d", len(children), requests.Load())
			}
			child := children[0]
			before := child.Seq()
			if shutdown {
				registry.Close()
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var followUp *tool.Tool
			for i := range tools {
				if tools[i].Name == "task_send" {
					followUp = &tools[i]
				}
			}
			if followUp == nil {
				t.Fatal("missing task_send tool")
			}
			_, err = followUp.Execute(ctx, map[string]any{"id": child.ID, "message": "continue"})
			if !errors.Is(err, context.Canceled) || child.Seq() != before || requests.Load() != 1 {
				t.Fatalf("error=%v sequence=%d (was %d) requests=%d", err, child.Seq(), before, requests.Load())
			}
		})
	}
}
