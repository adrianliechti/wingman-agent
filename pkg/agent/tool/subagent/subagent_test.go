package subagent_test

import (
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestAgentToolSchemaIncludesTypedSubagentParameters(t *testing.T) {
	agentTool := Tools(&agent.Config{}, nil)[0]

	required, ok := agentTool.Parameters["required"].([]string)
	if !ok {
		t.Fatalf("required has type %T", agentTool.Parameters["required"])
	}
	if !contains(required, "description") || !contains(required, "prompt") || !contains(required, "agent_type") {
		t.Fatalf("required = %#v, want description, prompt, and agent_type", required)
	}

	properties, ok := agentTool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties has type %T", agentTool.Parameters["properties"])
	}

	subagentType, ok := properties["agent_type"].(map[string]any)
	if !ok {
		t.Fatalf("agent_type property has type %T", properties["agent_type"])
	}

	enum, ok := subagentType["enum"].([]string)
	if !ok {
		t.Fatalf("agent_type enum has type %T", subagentType["enum"])
	}
	for _, name := range []string{
		"general-purpose",
		"explore",
		"verification",
		"security",
		"code-architect",
		"code-reviewer",
		"code-simplifier",
		"test-engineer",
	} {
		if !contains(enum, name) {
			t.Fatalf("agent_type enum = %#v, missing %q", enum, name)
		}
	}
}

func TestAgentToolRejectsInvalidExecuteInputs(t *testing.T) {
	agentTool := Tools(&agent.Config{}, nil)[0]

	cases := []struct {
		name    string
		args    map[string]any
		wantSub string
	}{
		{"missing description", map[string]any{"prompt": "p", "agent_type": "explore"}, "description is required"},
		{"blank description", map[string]any{"description": "   ", "prompt": "p", "agent_type": "explore"}, "description is required"},
		{"missing prompt", map[string]any{"description": "d", "agent_type": "explore"}, "prompt is required"},
		{"blank prompt", map[string]any{"description": "d", "prompt": "\t\n", "agent_type": "explore"}, "prompt is required"},
		{"missing agent_type", map[string]any{"description": "d", "prompt": "p"}, "agent_type is required"},
		{"unknown agent_type", map[string]any{"description": "d", "prompt": "p", "agent_type": "ninja"}, "unknown agent_type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := agentTool.Execute(t.Context(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want error containing %q", err, tc.wantSub)
			}
		})
	}
}

func TestAgentToolClassifiesEffectByAgentType(t *testing.T) {
	agentTool := Tools(&agent.Config{}, nil)[0]

	tests := []struct {
		name string
		args map[string]any
		want tool.Effect
	}{
		{"nil args", nil, tool.EffectDynamic},
		{"missing type", map[string]any{}, tool.EffectDynamic},
		{"general purpose", map[string]any{"agent_type": "general-purpose"}, tool.EffectMutates},
		{"explore", map[string]any{"agent_type": "explore"}, tool.EffectReadOnly},
		{"explore trims case", map[string]any{"agent_type": " Explore "}, tool.EffectReadOnly},
		{"verification", map[string]any{"agent_type": "verification"}, tool.EffectMutates},
		{"security", map[string]any{"agent_type": "security"}, tool.EffectReadOnly},
		{"code architect", map[string]any{"agent_type": "code-architect"}, tool.EffectReadOnly},
		{"code reviewer", map[string]any{"agent_type": "code-reviewer"}, tool.EffectReadOnly},
		{"code simplifier", map[string]any{"agent_type": "code-simplifier"}, tool.EffectMutates},
		{"test engineer", map[string]any{"agent_type": "test-engineer"}, tool.EffectMutates},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentTool.Effect(tt.args); got != tt.want {
				t.Fatalf("Effect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
