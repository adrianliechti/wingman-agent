package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

func TestHelpersValidateStatusAndAccountForUsage(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, text, wantError string
		structured                      bool
	}{
		{name: "completed", metadata: `"status":"completed",`, text: " ready "},
		{name: "status omitted", text: " ready "},
		{name: "partial text", metadata: `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},`, text: "partial", wantError: "max_output_tokens"},
		{name: "paused text", metadata: `"status":"completed","stop_reason":"pause_turn",`, text: "partial", wantError: "paused"},
		{name: "reasoning only", metadata: `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},`, wantError: "max_output_tokens"},
		{name: "filtered", metadata: `"status":"incomplete","incomplete_details":{"reason":"content_filter"},`, wantError: "content_filter"},
		{name: "failed", metadata: `"status":"failed","error":{"code":"server_error","message":"provider failure"},`, text: "partial", wantError: "provider failure"},
		{name: "cancelled", metadata: `"status":"cancelled",`, wantError: "cancelled"},
		{name: "in progress", metadata: `"status":"in_progress",`, text: "partial", wantError: "in_progress"},
		{name: "queued", metadata: `"status":"queued",`, wantError: "queued"},
		{name: "invalid JSON", metadata: `"status":"completed",`, text: "{", wantError: "decode structured model response", structured: true},
	} {
		for _, helper := range []string{"Generate", "Utility"} {
			if tc.structured && helper == "Utility" {
				continue
			}
			t.Run(helper+"/"+tc.name, func(t *testing.T) {
				textJSON, _ := json.Marshal(tc.text)
				body := fmt.Sprintf(`{"id":"resp_test","object":"response",%s"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}],"usage":{"input_tokens":12,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1},"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}}`, tc.metadata, textJSON)
				client := helperTestClient(body, nil)
				recorder := tracetest.NewSpanRecorder()
				provider := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))
				t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
				tel, err := telemetry.New(t.Context(), telemetry.Options{
					AgentName: "code-agent", ProviderName: "openai", TracerProvider: provider, DisableMetrics: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				cfg := &Config{client: &client, Telemetry: tel, MaxTaskTokens: 100}
				ctx, cancel := cfg.withTaskBudget(t.Context())
				defer cancel()
				var usage Usage
				var text string
				if helper == "Generate" {
					opts := GenerateOptions{Input: "test"}
					if tc.structured {
						opts.OutputSchema = map[string]any{"type": "object"}
					}
					result, generateErr := cfg.Generate(ctx, opts)
					text, usage, err = result.Text, result.Usage, generateErr
				} else {
					ctx = tool.WithUsageSink(ctx, func(delta tool.UsageDelta) {
						usage.InputTokens += delta.InputTokens
						usage.OutputTokens += delta.OutputTokens
						usage.ReasoningTokens += delta.ReasoningTokens
						usage.CacheReadInputTokens += delta.CacheReadInputTokens
						usage.CacheCreationInputTokens += delta.CacheCreationInputTokens
					})
					text, err = cfg.Utility(ctx, "helper", "test")
				}
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) || text != "" {
						t.Fatalf("text=%q err=%v; want empty text and %q error", text, err, tc.wantError)
					}
				} else if err != nil || text != strings.TrimSpace(tc.text) {
					t.Fatalf("text=%q err=%v", text, err)
				}
				if tc.name == "failed" {
					failure, ok := errors.AsType[*responseFailure](err)
					if !ok || failure.code != "server_error" {
						t.Fatalf("provider error code lost: %v", err)
					}
				}
				if usage.InputTokens != 12 || usage.OutputTokens != 4 || usage.ReasoningTokens != 1 || usage.CacheReadInputTokens != 3 || usage.CacheCreationInputTokens != 2 {
					t.Fatalf("usage=%+v", usage)
				}
				if used := ctx.Value(taskBudgetKey{}).(*taskBudget).used.Load(); used != 16 {
					t.Fatalf("task budget charged %d tokens, want 16", used)
				}
				spans := recorder.Ended()
				if len(spans) != 1 || (spans[0].Status().Code == codes.Error) != (err != nil) {
					t.Fatalf("telemetry must record the helper outcome: %v", spans)
				}
			})
		}
	}
}

func TestUtilityBoundsRequestIndependentlyOfMainModel(t *testing.T) {
	for _, modelID := range []string{"gpt-5.6-terra", "custom-model"} {
		t.Run(modelID, func(t *testing.T) {
			var request map[string]any
			client := helperTestClient(`{"status":"completed"}`, func(r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
			})
			cfg := &Config{client: &client, Model: func() string { return modelID }, OutputTokenBudget: 512, Effort: func() string { return "high" }}
			if _, err := cfg.Utility(t.Context(), "extract", "page"); err != nil {
				t.Fatal(err)
			}
			if request["model"] != modelID || request["max_output_tokens"] != float64(DefaultUtilityOutputTokenBudget) || request["store"] != false || request["truncation"] != "disabled" {
				t.Fatalf("utility request=%#v", request)
			}
			if modelID == "custom-model" {
				if _, ok := request["reasoning"]; ok {
					t.Fatalf("unknown model received unsupported reasoning options: %#v", request)
				}
			} else if reasoning, ok := request["reasoning"].(map[string]any); !ok || reasoning["effort"] != "low" {
				t.Fatalf("utility reasoning=%#v", request["reasoning"])
			}
		})
	}
}

func helperTestClient(body string, inspect func(*http.Request)) openai.Client {
	return openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL("http://helper.test"), option.WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if inspect != nil {
				inspect(r)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}),
	}))
}

func TestGenerateUsesStatelessStructuredRequestWithoutPromptCacheKey(t *testing.T) {
	var requestBody map[string]any
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &requestBody); err != nil {
			t.Fatal(err)
		}
		body := `{
            "id":"resp_1","object":"response","status":"completed",
            "output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"insert_text\":\"value\"}","annotations":[]}]}],
            "usage":{
              "input_tokens":12,
              "input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},
              "output_tokens":4,
              "output_tokens_details":{"reasoning_tokens":1},
              "total_tokens":16
            }
        }`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	client := openai.NewClient(
		option.WithBaseURL("http://generate.test"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(httpClient),
	)
	cfg := &Config{client: &client}

	result, err := cfg.Generate(context.Background(), GenerateOptions{
		Model:        "gpt-5.6-luna",
		Effort:       "none",
		Instructions: "complete code",
		Input:        "const value = ",
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"insert_text": map[string]any{"type": "string"},
			},
		},
		MaxOutputTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"insert_text":"value"}` {
		t.Fatalf("text = %q", result.Text)
	}
	if result.Usage.InputTokens != 12 ||
		result.Usage.OutputTokens != 4 ||
		result.Usage.ReasoningTokens != 1 ||
		result.Usage.CacheReadInputTokens != 3 ||
		result.Usage.CacheCreationInputTokens != 2 ||
		result.Usage.TotalTokens() != 16 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if requestBody["store"] != false || requestBody["max_output_tokens"] != float64(256) {
		t.Fatalf("stateless limits missing from request: %#v", requestBody)
	}
	if _, ok := requestBody["prompt_cache_key"]; ok {
		t.Fatalf("request contains prompt_cache_key: %#v", requestBody)
	}
	if _, ok := requestBody["tools"]; ok {
		t.Fatalf("request contains tools: %#v", requestBody["tools"])
	}
	text, ok := requestBody["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config = %#v", requestBody["text"])
	}
	format, ok := text["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("format = %#v", text["format"])
	}
	reasoning, ok := requestBody["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "none" {
		t.Fatalf("reasoning = %#v", requestBody["reasoning"])
	}
}
