package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/model"
)

func TestAgentSendsSupportedOutputTokenBudget(t *testing.T) {
	// Include non-tier limits to verify they retain their full allowance.
	original := model.Models
	t.Cleanup(func() { model.Models = original })
	model.Models = append(slices.Clone(original),
		model.Model{ID: "budget-64k", Output: 64_000},
		model.Model{ID: "budget-below-64k", Output: 63_999},
		model.Model{ID: "budget-48k", Output: 48_000},
		model.Model{ID: "budget-small", Output: 4_096},
		model.Model{ID: "budget-unspecified"},
	)

	for _, tc := range []struct {
		model  string
		budget int
		want   int
	}{
		{model: "gpt-6-astra", want: 64_000},
		{model: "gpt-6-astra", budget: 128_000, want: 128_000},
		{model: "gpt-6-astra", budget: 256_000, want: 128_000},
		{model: "gpt-6-astra", budget: 12_345, want: 12_345},
		{model: "openai/gpt-6-astra", want: 64_000},
		{model: "openai/gpt-6-astra", budget: 96_000, want: 96_000},
		{model: "qwen3.8:27b-mlx", want: 64_000},
		{model: "gpt-5.3-codex-spark", budget: 128_000, want: 32_000},
		{model: "budget-64k", want: 64_000},
		{model: "budget-below-64k", want: 63_999},
		{model: "budget-48k", want: 48_000},
		{model: "budget-small", want: 4_096},
		{model: "budget-unspecified"},
		{model: "budget-unspecified", budget: 32_000, want: 32_000},
		{model: "unknown-model"},
		{model: "unknown-model", budget: 32_000, want: 32_000},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.model, tc.budget), func(t *testing.T) {
			var body map[string]json.RawMessage
			client := streamingTestClient(func(r *http.Request) string {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				return phaseTestResponse(false, finalAnswerOutput)
			})
			cfg := &Config{client: &client, Model: func() string { return tc.model }, OutputTokenBudget: tc.budget}
			a := &Agent{Config: cfg.Derive()}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check the output allowance."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, present := body["max_output_tokens"]
			if tc.want == 0 {
				if present {
					t.Fatalf("unknown limit must omit max_output_tokens, got %s", raw)
				}
				return
			}
			var got int
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode max_output_tokens: %v", err)
			}
			if got != tc.want {
				t.Fatalf("max_output_tokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOutputTokenBudgetEnvironment(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("WINGMAN_TASK_MAX_TOKENS", "")
	t.Setenv("WINGMAN_TASK_TIMEOUT", "")
	for _, tc := range []struct {
		value   string
		want    int
		invalid bool
	}{
		{value: ""},
		{value: "0"},
		{value: " 96000 ", want: 96_000},
		{value: "1", want: 1},
		{value: "-1", invalid: true},
		{value: "bad", invalid: true},
		{value: "1.5", invalid: true},
		{value: "999999999999999999999999", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("WINGMAN_OUTPUT_TOKEN_BUDGET", tc.value)
			cfg, err := DefaultConfig()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "WINGMAN_OUTPUT_TOKEN_BUDGET") {
					t.Fatalf("invalid output budget %q: %v", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OutputTokenBudget != tc.want {
				t.Fatalf("output budget = %d, want %d", cfg.OutputTokenBudget, tc.want)
			}
		})
	}
}
