package prompt

import (
	"slices"
	"strings"
	"testing"
)

func allPromptTools() []string {
	return []string{
		"glob", "grep", "read", "edit",
		"exec", "exec_session", "schedule", "elicit",
		"fetch", "search_web", "lsp",
		"agent", "agent_output", "agent_send", "agent_stop",
	}
}

func TestPromptGuidanceFollowsAvailableTools(t *testing.T) {
	models := map[string]Variant{"fallback": VariantFor("")}
	for name, variant := range variants {
		models[name] = variant
	}
	for _, tools := range [][]string{nil, {"read", "glob", "grep"}, {"lsp"}, {"search_web"}, {"fetch"}, {"agent"}, allPromptTools()} {
		// File discovery and reading are always registered, including in Plan
		// mode and every supported child role. Only optional tools vary.
		tools = append(slices.Clone(tools), "read", "glob", "grep")
		for name, variant := range models {
			t.Run(name+"/"+strings.Join(tools, "+"), func(t *testing.T) {
				data := SectionData{ToolNames: tools, ModeInstructions: modePlan}
				for _, got := range []string{BuildBaseInstructions(variant.Agent, data), BuildAgentContext(data)} {
					for _, tool := range allPromptTools() {
						if !slices.Contains(tools, tool) && strings.Contains(got, "`"+tool+"`") {
							t.Errorf("guidance advertises unavailable tool %s", tool)
						}
					}
					for heading, want := range map[string]bool{
						"Delegation":        data.HasTools("agent"),
						"Web research":      data.HasTools("search_web") || data.HasTools("fetch"),
						"Code intelligence": data.HasTools("lsp"),
					} {
						if strings.Contains(got, "# "+heading+"\n") != want {
							t.Errorf("%s heading does not match tool availability", heading)
						}
					}
					if !strings.Contains(got, "Preserve unrelated user changes") || !strings.Contains(got, "# Plan mode") {
						t.Error("tool filtering removed general policy or active mode")
					}
					if strings.Contains(got, "{{") {
						t.Error("unrendered template syntax")
					}
				}
			})
		}
	}
}
