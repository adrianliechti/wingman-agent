package prompt

import (
	"strings"
	"testing"
)

func TestSharedCodeIntelligenceAcrossModelsAndContexts(t *testing.T) {
	for _, id := range []string{"unknown", "gpt-6.1-sol", "claude-sonnet-5-5", "gemini", "kimi"} {
		for _, build := range []func(string, SectionData) string{BuildBaseInstructions, BuildInstructions} {
			text := build(VariantFor(id).Agent, SectionData{})
			if strings.Count(text, "# Code intelligence\n") != 1 {
				t.Fatalf("%s: shared guidance missing or duplicated", id)
			}

		}
	}
	if !strings.Contains(BuildAgentContext(SectionData{}), "# Code intelligence\n") {
		t.Fatal("subagents did not inherit tool-selection guidance")
	}
}
