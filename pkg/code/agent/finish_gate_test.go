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
		"bedrock-opus-5-5":                     true,
		"bedrock-sonnet-5":                     true,
		"bedrock-haiku-4-5":                    true,
		"BEDROCK_OPUS_5_5":                     true,
		"bedrock.fable.5":                      true,
		"bedrock/mythos-5":                     true,
		"gpt-6-astra":                          false,
		"gemini-3-pro":                         false,
		"eu.gpt-5":                             false,
		"octopus":                              false,
		"affable-model":                        false,
		"claudette":                            false,
		"openai/gpt-6-astra:haiku-eval":        false,
		"qwen/qwen3.8-27b:sonnet-distill":      false,
	} {
		if got := requiresFinish(id); got != want {
			t.Errorf("requiresFinish(%q) = %t, want %t", id, got, want)
		}
	}
}
