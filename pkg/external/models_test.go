package external

import (
	"context"
	"strings"
	"testing"
)

func TestWithDefaultsDoesNotAssumeLocalServer(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")
	t.Setenv("WINGMAN_TOKEN", "")

	options := WithDefaults(nil)

	if options.WingmanURL != "" {
		t.Fatalf("WingmanURL = %q, want empty", options.WingmanURL)
	}
	if options.WingmanToken != "-" {
		t.Fatalf("WingmanToken = %q, want placeholder token", options.WingmanToken)
	}
}

func TestAvailableModelsRequiresWingmanURL(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")

	_, err := AvailableModels(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "WINGMAN_URL is required") {
		t.Fatalf("AvailableModels() error = %v, want missing WINGMAN_URL error", err)
	}
}

func TestProviderFilters(t *testing.T) {
	for _, tc := range []struct {
		id     string
		filter ModelFilter
		want   bool
	}{
		{"anthropic/claude-sonnet-5-5", IsAnthropic, true},
		{"~openai/GPT-6-ASTRA:latest", IsOpenAI, true},
		{"google/gemini-3.7-flash", IsGoogle, true},
		{"gpt-6-experimental", IsOpenAI, true},
		{"gptish-6", IsOpenAI, false},
		{"anthropic/gpt-6-astra", IsOpenAI, false},
		{"openai/claude-sonnet-5-5", IsAnthropic, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := tc.filter(tc.id); got != tc.want {
				t.Fatalf("filter(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
