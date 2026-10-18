package external

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type mcpCall struct {
	server, tool string
	input        map[string]any
}

func recordingMCP(calls *[]mcpCall, text string, err error) MCPCaller {
	return func(_ context.Context, server, tool string, input map[string]any) (string, error) {
		*calls = append(*calls, mcpCall{server, tool, input})
		return text, err
	}
}

func mcpPreToolUse(input map[string]any) *Config {
	return &Config{Hooks: Events{PreToolUse: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{{
		Type: "mcp_tool", Server: "policy", Tool: "check", Input: input,
	}}}}}}
}

func TestMCPToolHookExpandsTemplateAndAppliesDecision(t *testing.T) {
	var calls []mcpCall
	deny := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no force push"}}`
	cfg := mcpPreToolUse(map[string]any{
		"command": "${tool_input.command}",
		"timeout": "${tool_input.timeout}",
		"label":   "run ${tool_input.command} for ${session_id}",
		"nested":  []any{map[string]any{"tool": "${tool_name}"}},
	})
	hooks := cfg.BuildWithOptions(t.TempDir(), BuildOptions{MCP: recordingMCP(&calls, deny, nil)})
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "s1"})
	outcome, err := hooks.PreToolUse[0](ctx, tool.ToolCall{Name: "shell", Args: `{"command":"git push -f","timeout":30}`})
	if err != nil || !outcome.Block || outcome.Reason != "no force push" {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
	if len(calls) != 1 || calls[0].server != "policy" || calls[0].tool != "check" {
		t.Fatalf("calls = %#v", calls)
	}
	input := calls[0].input
	if input["command"] != "git push -f" || input["label"] != "run git push -f for s1" {
		t.Fatalf("input = %#v", input)
	}
	if timeout, ok := input["timeout"].(json.Number); !ok || timeout.String() != "30" {
		t.Fatalf("whole-string placeholder lost its JSON type: %#v", input["timeout"])
	}
	nested := input["nested"].([]any)[0].(map[string]any)
	if nested["tool"] != "Bash" {
		t.Fatalf("nested = %#v", nested)
	}
}

func TestMCPToolHookFailuresFailOpen(t *testing.T) {
	for name, options := range map[string]BuildOptions{
		"tool error":    {MCP: recordingMCP(new([]mcpCall), "", errors.New("MCP tool returned an error: boom"))},
		"no MCP caller": {},
	} {
		t.Run(name, func(t *testing.T) {
			hooks := mcpPreToolUse(nil).BuildWithOptions(t.TempDir(), options)
			outcome, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell", Args: `{}`})
			if err != nil || outcome.Block {
				t.Fatalf("outcome = %+v, err = %v", outcome, err)
			}
		})
	}

	var calls []mcpCall
	hooks := mcpPreToolUse(map[string]any{"x": "${tool_input.missing}"}).BuildWithOptions(t.TempDir(), BuildOptions{MCP: recordingMCP(&calls, "", nil)})
	outcome, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell", Args: `{}`})
	if err != nil || outcome.Block || len(calls) != 0 {
		t.Fatalf("missing placeholder must fail the hook before calling: %+v, %v, %d calls", outcome, err, len(calls))
	}
}

func TestMCPToolHookIsSkippedForSessionEndAndNeverAsync(t *testing.T) {
	var calls []mcpCall
	caller := recordingMCP(&calls, "", nil)
	sessionEnd := &Config{Hooks: Events{SessionEnd: []MatcherGroup{{Hooks: []Handler{{Type: "mcp_tool", Server: "s", Tool: "t"}}}}}}
	sessionEnd.BuildWithOptions(t.TempDir(), BuildOptions{MCP: caller}).SessionEnd[0](t.Context(), "other")
	if len(calls) != 0 {
		t.Fatal("SessionEnd mcp_tool hook ran")
	}

	async := NewAsync(nil)
	defer async.Close()
	cfg := &Config{Hooks: Events{UserPromptSubmit: []MatcherGroup{{Hooks: []Handler{{Type: "mcp_tool", Server: "s", Tool: "t", Async: true}}}}}}
	outcome, err := cfg.BuildWithOptions(t.TempDir(), BuildOptions{MCP: recordingMCP(&calls, "policy context", nil), Async: async}).UserPromptSubmit[0](t.Context(), "hi")
	if err != nil || len(calls) != 1 || len(outcome.AdditionalContext) != 1 || outcome.AdditionalContext[0] != "policy context" {
		t.Fatalf("mcp_tool hook should run synchronously: %+v, %v, %d calls", outcome, err, len(calls))
	}
}
