package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestVerificationToolFilterRejectsUnknownAndMutatingTools(t *testing.T) {
	tests := []struct {
		name string
		tool tool.Tool
		want bool
	}{
		{
			name: "read only tool",
			tool: tool.Tool{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
			want: true,
		},
		{
			name: "shell allowed for tests",
			tool: tool.Tool{Name: "shell", Effect: func(map[string]any) tool.Effect { return tool.EffectDynamic }},
			want: true,
		},
		{
			name: "unknown effect rejected",
			tool: tool.Tool{Name: "mcp_unknown"},
			want: false,
		},
		{
			name: "mutating tool rejected",
			tool: tool.Tool{Name: "schedule_task", Effect: tool.StaticEffect(tool.EffectMutates)},
			want: false,
		},
		{
			name: "write rejected by name",
			tool: tool.Tool{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowVerificationTool(tt.tool); got != tt.want {
				t.Fatalf("allowVerificationTool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowReadOnlyTool(t *testing.T) {
	tests := []struct {
		name string
		tool tool.Tool
		want bool
	}{
		{"read-only allowed", tool.Tool{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)}, true},
		{"dynamic allowed (will be wrapped)", tool.Tool{Name: "shell", Effect: tool.StaticEffect(tool.EffectDynamic)}, true},
		{"mutating rejected", tool.Tool{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)}, false},
		{"ask_user rejected by name", tool.Tool{Name: "ask_user", Effect: tool.StaticEffect(tool.EffectReadOnly)}, false},
		{"agent rejected by name", tool.Tool{Name: "agent", Effect: tool.StaticEffect(tool.EffectReadOnly)}, false},
		{"hidden rejected", tool.Tool{Name: "x", Hidden: true, Effect: tool.StaticEffect(tool.EffectReadOnly)}, false},
		{"missing effect rejected", tool.Tool{Name: "x"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowReadOnlyTool(tt.tool); got != tt.want {
				t.Fatalf("allowReadOnlyTool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowStaticSecurityToolRejectsNetworkTools(t *testing.T) {
	tests := []struct {
		name string
		tool tool.Tool
		want bool
	}{
		{"read-only allowed", tool.Tool{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)}, true},
		{"shell dynamic allowed for wrapping", tool.Tool{Name: "shell", Effect: tool.StaticEffect(tool.EffectDynamic)}, true},
		{"web fetch rejected", tool.Tool{Name: "web_fetch", Effect: tool.StaticEffect(tool.EffectReadOnly)}, false},
		{"web search rejected", tool.Tool{Name: "web_search", Effect: tool.StaticEffect(tool.EffectReadOnly)}, false},
		{"write rejected", tool.Tool{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowStaticSecurityTool(tt.tool); got != tt.want {
				t.Fatalf("allowStaticSecurityTool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowNonAgentTool(t *testing.T) {
	if allowNonAgentTool(tool.Tool{Name: "agent"}) {
		t.Error("agent must be rejected")
	}
	if allowNonAgentTool(tool.Tool{Name: "x", Hidden: true}) {
		t.Error("hidden tools must be rejected")
	}
	if !allowNonAgentTool(tool.Tool{Name: "read"}) {
		t.Error("ordinary tool must pass")
	}
}

func TestToolsForTypeFiltersExplore(t *testing.T) {
	all := []tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "shell", Effect: tool.StaticEffect(tool.EffectDynamic), Execute: func(context.Context, map[string]any) (string, error) { return "ok", nil }},
		{Name: "ask_user", Hidden: true, Effect: tool.StaticEffect(tool.EffectReadOnly)},
	}

	filtered := toolsForType(all, subagentTypes["explore"])

	names := toolNames(filtered)
	if !containsName(names, "read") || !containsName(names, "shell") {
		t.Fatalf("explore must keep read + shell, got %v", names)
	}
	if containsName(names, "write") {
		t.Errorf("explore must reject mutating tools, got %v", names)
	}
	if containsName(names, "ask_user") {
		t.Errorf("explore must reject ask_user, got %v", names)
	}
}

func TestExploreWrapsDynamicToolsAsReadOnly(t *testing.T) {
	called := false
	dynamic := tool.Tool{
		Name: "shell",
		Effect: func(args map[string]any) tool.Effect {
			// nil args → classify as Dynamic so allowReadOnlyTool accepts the
			// tool for wrapping (matches the real shell tool's behavior).
			if args == nil {
				return tool.EffectDynamic
			}
			if v, _ := args["safe"].(bool); v {
				return tool.EffectReadOnly
			}
			return tool.EffectMutates
		},
		Execute: func(context.Context, map[string]any) (string, error) {
			called = true
			return "ran", nil
		},
	}

	filtered := toolsForType([]tool.Tool{dynamic}, subagentTypes["explore"])
	if len(filtered) != 1 {
		t.Fatalf("len(filtered) = %d, want 1", len(filtered))
	}
	wrapped := filtered[0]

	// Read-only path: original executor runs.
	out, err := wrapped.Execute(context.Background(), map[string]any{"safe": true})
	if err != nil || out != "ran" {
		t.Fatalf("read-only call: got (%q, %v), want (ran, nil)", out, err)
	}
	if !called {
		t.Error("original executor must have run on read-only path")
	}

	// Mutating path: blocked.
	called = false
	_, err = wrapped.Execute(context.Background(), map[string]any{"safe": false})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("mutating call: want read-only refusal, got %v", err)
	}
	if called {
		t.Error("original executor must NOT have run on mutating path")
	}
}

func TestToolsForTypeGeneralPurposeKeepsMutating(t *testing.T) {
	all := []tool.Tool{
		{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "agent", Effect: tool.StaticEffect(tool.EffectMutates)},
	}
	filtered := toolsForType(all, subagentTypes["general-purpose"])

	names := toolNames(filtered)
	if !containsName(names, "write") {
		t.Errorf("general-purpose must keep write, got %v", names)
	}
	if containsName(names, "agent") {
		t.Errorf("general-purpose must reject recursive agent tool, got %v", names)
	}
}

func TestSpecializedReadOnlyAgentsFilterLikeExplore(t *testing.T) {
	all := []tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "shell", Effect: tool.StaticEffect(tool.EffectDynamic), Execute: func(context.Context, map[string]any) (string, error) { return "ok", nil }},
	}

	for _, name := range []string{"code-explorer", "code-architect", "code-reviewer", "legacy-analyst"} {
		t.Run(name, func(t *testing.T) {
			filtered := toolsForType(all, subagentTypes[name])
			names := toolNames(filtered)
			if !containsName(names, "read") || !containsName(names, "shell") {
				t.Fatalf("%s must keep read + shell, got %v", name, names)
			}
			if containsName(names, "write") {
				t.Fatalf("%s must reject write, got %v", name, names)
			}
		})
	}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
