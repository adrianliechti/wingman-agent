package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestAgentToolSchemaIncludesTypedSubagentParameters(t *testing.T) {
	agentTool := Tools(&agent.Config{})[0]

	required, ok := agentTool.Parameters["required"].([]string)
	if !ok {
		t.Fatalf("required has type %T", agentTool.Parameters["required"])
	}
	if !contains(required, "description") || !contains(required, "prompt") {
		t.Fatalf("required = %#v, want description and prompt", required)
	}

	properties, ok := agentTool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties has type %T", agentTool.Parameters["properties"])
	}

	subagentType, ok := properties["subagent_type"].(map[string]any)
	if !ok {
		t.Fatalf("subagent_type property has type %T", properties["subagent_type"])
	}

	enum, ok := subagentType["enum"].([]string)
	if !ok {
		t.Fatalf("subagent_type enum has type %T", subagentType["enum"])
	}
	for _, name := range []string{"general-purpose", "explore", "verification"} {
		if !contains(enum, name) {
			t.Fatalf("subagent_type enum = %#v, missing %q", enum, name)
		}
	}
}

func TestClassifyEffect(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want tool.Effect
	}{
		{"nil args", nil, tool.EffectDynamic},
		{"default", map[string]any{}, tool.EffectMutates},
		{"general purpose", map[string]any{"subagent_type": "general-purpose"}, tool.EffectMutates},
		{"explore", map[string]any{"subagent_type": "explore"}, tool.EffectReadOnly},
		{"explore trims case", map[string]any{"subagent_type": " Explore "}, tool.EffectReadOnly},
		{"verification", map[string]any{"subagent_type": "verification"}, tool.EffectMutates},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyEffect(tt.args); got != tt.want {
				t.Fatalf("classifyEffect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentToolValidatesRequiredArguments(t *testing.T) {
	agentTool := Tools(&agent.Config{})[0]

	_, err := agentTool.Execute(context.Background(), map[string]any{
		"prompt": "Find auth middleware",
	})
	if err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("expected description validation error, got: %v", err)
	}

	_, err = agentTool.Execute(context.Background(), map[string]any{
		"description": "Find auth",
		"prompt":      "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("expected prompt validation error, got: %v", err)
	}
}

func TestToolsForExploreFiltersAndWrapsTools(t *testing.T) {
	calledShell := false
	tools := toolsForType([]tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "edit", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "ask_user", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "hidden", Hidden: true, Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "agent", Effect: classifyEffect},
		{
			Name: "shell",
			Effect: func(args map[string]any) tool.Effect {
				if args == nil {
					return tool.EffectDynamic
				}
				command, _ := args["command"].(string)
				if strings.Contains(command, ">") {
					return tool.EffectMutates
				}
				return tool.EffectReadOnly
			},
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				calledShell = true
				return "ok", nil
			},
		},
	}, subagentTypes["explore"])

	names := toolNames(tools)
	if !contains(names, "read") || !contains(names, "shell") {
		t.Fatalf("explore tools = %#v, want read and shell", names)
	}
	for _, disallowed := range []string{"edit", "ask_user", "hidden", "agent"} {
		if contains(names, disallowed) {
			t.Fatalf("explore tools = %#v, should not include %q", names, disallowed)
		}
	}

	shellTool := findToolForTest(tools, "shell")
	if shellTool == nil {
		t.Fatal("shell tool missing")
	}

	if _, err := shellTool.Execute(context.Background(), map[string]any{"command": "git status"}); err != nil {
		t.Fatalf("read-only dynamic tool call rejected: %v", err)
	}
	if !calledShell {
		t.Fatal("read-only dynamic tool did not call wrapped executor")
	}

	calledShell = false
	_, err := shellTool.Execute(context.Background(), map[string]any{"command": "echo hi > file.txt"})
	if err == nil || !strings.Contains(err.Error(), "only allows read-only shell calls") {
		t.Fatalf("mutating dynamic tool call was not rejected: %v", err)
	}
	if calledShell {
		t.Fatal("mutating dynamic tool reached wrapped executor")
	}
}

func TestToolsForVerificationFiltersEditingTools(t *testing.T) {
	tools := toolsForType([]tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "shell", Effect: func(map[string]any) tool.Effect { return tool.EffectDynamic }},
		{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "edit", Effect: tool.StaticEffect(tool.EffectMutates)},
		{Name: "ask_user", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		{Name: "agent", Effect: classifyEffect},
	}, subagentTypes["verification"])

	names := toolNames(tools)
	if !contains(names, "read") || !contains(names, "shell") {
		t.Fatalf("verification tools = %#v, want read and shell", names)
	}
	for _, disallowed := range []string{"write", "edit", "ask_user", "agent"} {
		if contains(names, disallowed) {
			t.Fatalf("verification tools = %#v, should not include %q", names, disallowed)
		}
	}
}

func toolNames(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func findToolForTest(tools []tool.Tool, name string) *tool.Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
