package harness

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestToolboxRunsNestedCallsUnderHooks(t *testing.T) {
	large := strings.Repeat("x", 4*maxInlineToolResultBytes)
	var mu sync.Mutex
	var hooked, progressed []string
	var nestedToolbox tool.Toolbox = unset{}
	var approving tool.ToolCall

	tools := []tool.Tool{
		{Name: "outer", Composite: true, Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
			box := tool.ToolboxFromContext(ctx)
			var names []string
			for _, peer := range box.Tools() {
				names = append(names, peer.Name)
			}
			inner := box.Call(ctx, "inner", map[string]any{"n": 1})
			blocked := box.Call(ctx, "blocked", nil)
			self := box.Call(ctx, "outer", nil)
			missing := box.Call(ctx, "nope", nil)
			asked := box.Call(ctx, "ask", nil)
			return tool.Text(strings.Join([]string{
				strings.Join(names, ","),
				// The nested result arrives whole; only the caller's is bounded.
				map[bool]string{true: "whole", false: "cut"}[inner.Content.Text() == large],
				blocked.Content.Text(), self.Content.Text(), missing.Content.Text(), asked.Content.Text(),
			}, "|")), nil
		}},
		{Name: "inner", Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			nestedToolbox = tool.ToolboxFromContext(ctx)
			// What an approval raised here is judged by: this call, not the caller's.
			approving, _ = hook.ToolCallFromContext(ctx)
			if report := tool.Progress(ctx); report != nil {
				report("inner working")
			}
			return tool.Text(large), nil
		}},
		{Name: "blocked", Execute: func(context.Context, map[string]any) (tool.Result, error) {
			t.Error("a call the hook blocked ran")
			return tool.Text("ran"), nil
		}},
		// Answered by someone watching the conversation, who never sees a nested call.
		{Name: "ask", Interactive: true, Execute: func(context.Context, map[string]any) (tool.Result, error) {
			t.Error("an interactive tool ran nested")
			return tool.Text("ran"), nil
		}},
	}
	a := &Agent{Config: &Config{Hooks: hook.Hooks{
		PreToolUse: []hook.PreToolUse{func(_ context.Context, call tool.ToolCall) (hook.PreToolUseOutcome, error) {
			mu.Lock()
			hooked = append(hooked, call.ID+":"+call.Name+":"+call.Args)
			mu.Unlock()
			return hook.PreToolUseOutcome{Outcome: hook.Outcome{Block: call.Name == "blocked", Reason: "not allowed"}}, nil
		}},
		TransformToolResult: []hook.TransformToolResult{func(_ context.Context, call tool.ToolCall, content string) (string, error) {
			if strings.Contains(call.ID, ".") {
				t.Errorf("transformed the nested result of %s", call.Name)
			}
			return content, nil
		}},
	}}}
	ctx := tool.WithProgressSink(t.Context(), func(_ context.Context, callID, text string) {
		progressed = append(progressed, callID+":"+text)
	})

	result := a.runSingleToolCall(ctx, ToolCall{ID: "c1", Name: "outer", Args: `{}`}, tools)
	want := "inner,blocked|whole|error: not allowed|error: outer cannot call itself|error: unknown tool nope|error: ask can only be called directly"
	if result.IsError || result.Content.Text() != want {
		t.Fatalf("result = %q", result.Content.Text())
	}
	if got := strings.Join(hooked, " "); got != `c1:outer:{} c1.1:inner:{"n":1} c1.2:blocked:{} c1.3:nope:{}` {
		t.Fatalf("hooked = %s", got)
	}
	// A nested call reports under its caller and cannot nest further.
	if len(progressed) != 1 || progressed[0] != "c1:inner working" {
		t.Fatalf("progress = %v", progressed)
	}
	if nestedToolbox != nil {
		t.Fatalf("nested call got a toolbox: %v", nestedToolbox)
	}
	if approving.ID != "c1.1" || approving.Name != "inner" || approving.Args != `{"n":1}` {
		t.Fatalf("approval context = %+v", approving)
	}
	// Nested calls leave no trace in the conversation.
	for _, event := range a.Events {
		if event.Tool != nil && event.Tool.Name != "outer" {
			t.Fatalf("recorded nested call: %+v", event.Tool)
		}
	}

	// Only a composite tool is handed the others.
	nestedToolbox = unset{}
	if plain := a.runSingleToolCall(ctx, ToolCall{ID: "c2", Name: "inner", Args: `{}`}, tools); plain.IsError || nestedToolbox != nil {
		t.Fatalf("a plain tool got a toolbox: %v", nestedToolbox)
	}
}

type unset struct{ tool.Toolbox }
