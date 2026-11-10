package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// sendCancellingOnRetry runs one turn and cancels at the first announced
// retry, so the policy is observed without sleeping through its backoff.
func sendCancellingOnRetry(t *testing.T, a *harness.Agent) ([]harness.RetryInfo, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var retries []harness.RetryInfo
	ctx = harness.WithStreamEventHandlers(ctx, harness.StreamEventHandlers{Retry: func(info harness.RetryInfo) {
		retries = append(retries, info)
		cancel()
	}})
	stream, err := a.Send(ctx, []harness.Content{{Text: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		runErr = err
	}
	return retries, runErr
}

func sseEvent(w http.ResponseWriter, event any) {
	encoded, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
}

// The Responses API reports stream failures as an error event with a nested
// error object. openai-go surfaces that shape as a generic stream error, which
// must not be mistaken for a dropped connection. The terminal codes and the
// header cases follow Codex's codex-api sse/responses_error_tests.rs; unknown
// codes are terminal here by design, where Codex retries them.
func TestProviderErrorE2ENestedErrorEvent(t *testing.T) {
	const advice = "Please try again in 9 seconds."
	for _, tc := range []struct {
		name               string
		detail             map[string]any
		terminal           bool
		minDelay, maxDelay int64
		reason             string
	}{
		{"header overrides message", map[string]any{"code": "rate_limit_exceeded", "message": advice, "headers": map[string]any{"Retry-After": "12"}}, false, 12000, 12000, "Provider rate limit"},
		{"lowercase numeric header", map[string]any{"code": "slow_down", "message": advice, "headers": map[string]any{"retry-after": 12}}, false, 12000, 12000, "Provider rate limit"},
		{"invalid header falls back", map[string]any{"code": "rate_limit_exceeded", "message": advice, "headers": map[string]any{"Retry-After": "invalid"}}, false, 9000, 9000, "Provider rate limit"},
		{"malformed headers fall back", map[string]any{"code": "rate_limit_exceeded", "message": advice, "headers": "invalid"}, false, 9000, 9000, "Provider rate limit"},
		{"overloaded", map[string]any{"code": "server_is_overloaded", "headers": map[string]any{"Retry-After": "12"}}, false, 12000, 12000, ""},
		{"server error type without code", map[string]any{"type": "server_error", "code": nil, "message": "The server had an error."}, false, 1000, 2000, ""},
		{"Anthropic overload type", map[string]any{"type": "overloaded_error", "message": "Overloaded"}, false, 1000, 2000, ""},
		{"status as code", map[string]any{"code": 503, "message": "The model is overloaded.", "status": "UNAVAILABLE"}, false, 1000, 2000, ""},

		{"quota", map[string]any{"code": "insufficient_quota", "headers": map[string]any{"retry-after": "1"}}, true, 0, 0, ""},
		{"usage limit", map[string]any{"code": "organization_usage_limit_exceeded", "headers": map[string]any{"retry-after": "1"}}, true, 0, 0, ""},
		{"cyber policy", map[string]any{"code": "cyber_policy", "headers": map[string]any{"retry-after": "1"}}, true, 0, 0, ""},
		{"bio policy", map[string]any{"code": "bio_policy", "headers": map[string]any{"retry-after": "1"}}, true, 0, 0, ""},
		{"content policy", map[string]any{"code": "content_policy_violation", "plan_type": 42, "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"budget reached", map[string]any{"code": "budget_reached", "plan_type": 42, "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"flex unavailable", map[string]any{"code": "flex_unavailable", "plan_type": 42, "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"invalid access program", map[string]any{"code": "invalid_access_program", "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"unsupported access program", map[string]any{"code": "unsupported_access_program", "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"access program not enabled", map[string]any{"code": "access_program_not_enabled", "headers": map[string]any{"retry-after": "300"}}, true, 0, 0, ""},
		{"model not found", map[string]any{"code": "model_not_found"}, true, 0, 0, ""},
		{"invalid prompt", map[string]any{"code": "invalid_prompt", "message": "Invalid request."}, true, 0, 0, ""},
		{"invalid request type without code", map[string]any{"type": "invalid_request_error", "code": nil, "message": "This model is unavailable.", "param": "model"}, true, 0, 0, ""},
		{"unknown code", map[string]any{"code": "unknown_failure", "headers": map[string]any{"retry-after": "7"}}, true, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
				sseEvent(w, map[string]any{"type": "error", "sequence_number": 1, "error": tc.detail})
				// The failure ends the stream; a later completion is not consumed.
				fmt.Fprint(w, completedText("must not be used"))
			})
			a := &harness.Agent{Config: e2eConfig(t)}
			retries, runErr := sendCancellingOnRetry(t, a)
			if len(p.snapshot()) != 1 {
				t.Fatalf("requests = %d", len(p.snapshot()))
			}
			if len(a.MessagesSnapshot()) != 1 {
				t.Fatalf("failed stream changed history: %+v", a.MessagesSnapshot())
			}
			if tc.terminal {
				name := tc.detail["code"]
				if name == nil {
					name = tc.detail["type"]
				}
				if len(retries) != 0 || runErr == nil || errors.Is(runErr, context.Canceled) || !strings.Contains(runErr.Error(), fmt.Sprint(name)) {
					t.Fatalf("terminal error=%v retries=%+v", runErr, retries)
				}
				if message, ok := tc.detail["message"].(string); ok && !strings.Contains(runErr.Error(), message) {
					t.Fatalf("provider message lost: %v", runErr)
				}
				return
			}
			if len(retries) != 1 || !errors.Is(runErr, context.Canceled) {
				t.Fatalf("error=%v retries=%+v", runErr, retries)
			}
			if got := retries[0]; got.Attempt != 1 || got.DelayMillis < tc.minDelay || got.DelayMillis > tc.maxDelay || (tc.reason != "" && got.Reason != tc.reason) {
				t.Fatalf("retry = %+v, want delay %d..%d reason %q", got, tc.minDelay, tc.maxDelay, tc.reason)
			}
		})
	}
}

// A provider's own transient vocabulary reaches the agent as the code of an
// in-band failure. It must back off and retry like OpenAI's.
func TestProviderErrorE2EProviderTransientCodes(t *testing.T) {
	for _, code := range []string{"overloaded_error", "api_error", "rate_limit_error", "timeout_error", "service_unavailable_error", "throttlingException", "UNAVAILABLE"} {
		for _, transport := range []string{"error", "response.failed"} {
			t.Run(code+"/"+transport, func(t *testing.T) {
				p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
					if transport == "error" {
						sseEvent(w, map[string]any{"type": "error", "code": code, "message": "try later"})
						return
					}
					sseEvent(w, map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": code, "message": "try later"}}})
				})
				retries, runErr := sendCancellingOnRetry(t, &harness.Agent{Config: e2eConfig(t)})
				if len(retries) != 1 || retries[0].DelayMillis < 1000 || !errors.Is(runErr, context.Canceled) || len(p.snapshot()) != 1 {
					t.Fatalf("error=%v retries=%+v requests=%d", runErr, retries, len(p.snapshot()))
				}
			})
		}
	}
	for _, transport := range []string{"http", "error", "response.failed"} {
		t.Run("organization_usage_limit_exceeded/"+transport, func(t *testing.T) {
			const code = "organization_usage_limit_exceeded"
			failure := map[string]any{"code": code, "message": "Usage limit reached"}
			p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
				switch transport {
				case "http":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": failure})
				case "error":
					sseEvent(w, map[string]any{"type": "error", "code": code, "message": "Usage limit reached"})
				default:
					sseEvent(w, map[string]any{"type": "response.failed", "response": map[string]any{"error": failure}})
				}
			})
			retries, runErr := sendCancellingOnRetry(t, &harness.Agent{Config: e2eConfig(t)})
			if runErr == nil || !strings.Contains(runErr.Error(), code) || len(retries) != 0 || len(p.snapshot()) != 1 {
				t.Fatalf("error=%v retries=%+v requests=%d", runErr, retries, len(p.snapshot()))
			}
		})
	}
}

// Some providers and proxies answer with {"error":"..."}. openai-go cannot
// decode that into an API error, so the status and the text have to be
// recovered for the failure to be classified and explained.
func TestProviderErrorE2EStringErrorBody(t *testing.T) {
	for _, tc := range []struct {
		status   int
		message  string
		terminal bool
		reason   string
	}{
		{http.StatusUnauthorized, "unauthorized", true, ""},
		{http.StatusBadRequest, "unsupported parameter: store", true, ""},
		{http.StatusNotFound, "model 'test-model' not found", true, ""},
		{http.StatusTooManyRequests, "rate limit reached", false, "Provider rate limit"},
		{http.StatusServiceUnavailable, "upstream unavailable", false, "Provider temporarily unavailable"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.message})
			})
			retries, runErr := sendCancellingOnRetry(t, &harness.Agent{Config: e2eConfig(t)})
			if len(p.snapshot()) != 1 {
				t.Fatalf("requests = %d", len(p.snapshot()))
			}
			if tc.terminal {
				if len(retries) != 0 || runErr == nil || !strings.Contains(runErr.Error(), fmt.Sprint(tc.status)) || !strings.Contains(runErr.Error(), tc.message) {
					t.Fatalf("terminal error=%v retries=%+v", runErr, retries)
				}
				return
			}
			if len(retries) != 1 || retries[0].Reason != tc.reason || retries[0].DelayMillis != 7000 || !errors.Is(runErr, context.Canceled) {
				t.Fatalf("error=%v retries=%+v", runErr, retries)
			}
		})
	}
}

func TestProviderErrorE2EHTTPOverflowRecoversThroughCompaction(t *testing.T) {
	const anthropic = "input length and `max_tokens` exceed context limit: 188240 + 21333 > 200000, decrease input length or `max_tokens` and try again"
	for _, tc := range []struct {
		name   string
		status int
		body   any
	}{
		{"Anthropic wording", http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": anthropic}}},
		{"Gemini wording", http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "status": "INVALID_ARGUMENT", "message": "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575)."}}},
		{"xAI wording", http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "This model's maximum prompt length is 131072 but the request contains 537812 tokens."}}},
		{"llama.cpp wording", http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "type": "exceed_context_size_error", "message": "the request exceeds the available context size, try increasing it"}}},
		{"Bedrock wording", http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Too many input tokens"}}},
		{"string error body", http.StatusBadRequest, map[string]any{"error": "prompt is too long: 250000 tokens > 200000 maximum"}},
		{"413 without body", http.StatusRequestEntityTooLarge, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, summaries atomic.Int64
			p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if isCheckpointRequest(req) {
					summaries.Add(1)
					fmt.Fprint(w, completedText("Earlier work preserved"))
					return
				}
				if !req.Stream {
					t.Error("fell back to the utility briefing")
					return
				}
				if requests.Add(1) == 1 {
					if tc.body == nil {
						w.Header().Del("Content-Type")
						w.WriteHeader(tc.status)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(tc.body)
					return
				}
				fmt.Fprint(w, completedText("continued"))
			})
			cfg := e2eConfig(t)
			cfg.ContextWindow = -1
			a := &harness.Agent{Config: cfg, Messages: []harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{{Text: strings.Repeat("observed progress ", 1000)}}}}}
			if err := drain(t, a, "continue the task"); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 || summaries.Load() != 1 || !bytes.Contains(p.snapshot()[2].Input, []byte("Earlier work preserved")) {
				t.Fatalf("overflow did not recover through compaction: requests=%d summaries=%d", requests.Load(), summaries.Load())
			}
		})
	}
}

// Compaction cannot shrink a request whose newest input alone is too large.
// A 413 that persists must end the turn instead of compacting repeatedly.
func TestProviderErrorE2EPersistentPayloadTooLargeEndsTurn(t *testing.T) {
	var requests, summaries atomic.Int64
	newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if isCheckpointRequest(req) {
			summaries.Add(1)
			fmt.Fprint(w, completedText("Earlier work preserved"))
			return
		}
		requests.Add(1)
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow = -1
	a := &harness.Agent{Config: cfg, Messages: []harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{{Text: strings.Repeat("observed progress ", 1000)}}}}}
	if err := drain(t, a, "continue the task"); err == nil {
		t.Fatal("persistent 413 was hidden")
	}
	if requests.Load() > 3 || summaries.Load() > 3 || a.StateSnapshot().ContextRevision > 1 {
		t.Fatalf("requests=%d summaries=%d revision=%d", requests.Load(), summaries.Load(), a.StateSnapshot().ContextRevision)
	}
}

// An output-limit rejection is not an overflow: compacting would rewrite
// history without fixing the request.
func TestProviderErrorE2EOutputLimitDoesNotCompact(t *testing.T) {
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if isCheckpointRequest(req) || !req.Stream {
			t.Error("output-limit rejection triggered compaction")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for this model"}}`)
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow = -1
	a := &harness.Agent{Config: cfg, Messages: []harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{{Text: strings.Repeat("observed progress ", 1000)}}}}}
	retries, runErr := sendCancellingOnRetry(t, a)
	if runErr == nil || !strings.Contains(runErr.Error(), "maximum allowed number of output tokens") || len(retries) != 0 || len(p.snapshot()) != 1 || a.StateSnapshot().ContextRevision != 0 {
		t.Fatalf("error=%v retries=%+v requests=%d", runErr, retries, len(p.snapshot()))
	}
}
