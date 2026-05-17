package subagent

import (
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
