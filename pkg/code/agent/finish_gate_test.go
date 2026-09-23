package agent

import "testing"

func TestRequiresFinish(t *testing.T) {
	for id, want := range map[string]bool{
		"claude-sonnet-5":                      true,
		"eu.anthropic.claude-sonnet-5-v1:0":    true,
		"anthropic/claude-opus-5":              true,
		"anthropic/claude-opus-5-5":            true,
		"Anthropic-Claude":                     true,
		"bedrock/eu.anthropic.claude-opus-4-8": true,
		"gpt-6-astra":                          false,
		"gemini-3-pro":                         false,
		"eu.gpt-5":                             false,
	} {
		if got := requiresFinish(id); got != want {
			t.Errorf("requiresFinish(%q) = %t, want %t", id, got, want)
		}
	}
}
