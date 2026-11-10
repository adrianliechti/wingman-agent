package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// localProvider serves one fixed reply through the same client construction
// as createClient, so SDK decoding of the reply is the real one.
func localProvider(t *testing.T, respond http.HandlerFunc) *Config {
	t.Helper()
	server := httptest.NewServer(respond)
	t.Cleanup(server.Close)
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	return &Config{client: &client}
}

func sseEvents(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}
}

func completeOnce(t *testing.T, cfg *Config) error {
	t.Helper()
	_, err := cfg.complete(t.Context(), &request{model: "m"}, func(Message, error) bool { return true })
	if err == nil {
		t.Fatal("request succeeded")
	}
	return err
}

// openai-go ends the stream with a generic error for any event carrying a
// top-level "error" member. The provider's code, message and retry advice
// must survive that, in the shapes Codex parses (codex-api sse/responses.rs).
func TestStreamedErrorEventsBecomeProviderFailures(t *testing.T) {
	const delta = `{"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"partial"}`
	const completed = `{"type":"response.completed","response":{"id":"later"}}`
	for _, tc := range []struct {
		name   string
		events []string
		want   responseFailure
	}{
		{"nested error object", []string{
			`{"type":"error","sequence_number":2,"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model.","param":"input"}}`,
		}, responseFailure{code: "context_length_exceeded", message: "Your input exceeds the context window of this model."}},
		{"type names the class when code is null", []string{
			`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"This model is unavailable.","code":null,"param":"model"}}`,
		}, responseFailure{code: "invalid_request_error", message: "This model is unavailable."}},
		{"retry advice in error headers", []string{
			`{"type":"error","error":{"code":"rate_limit_exceeded","message":"Try again in 35 seconds.","headers":{"retry-after":"300"}}}`,
		}, responseFailure{code: "rate_limit_exceeded", message: "Try again in 35 seconds.", retryAfter: "300"}},
		{"numeric retry header and malformed extra field", []string{
			`{"type":"error","error":{"code":"server_is_overloaded","plan_type":42,"headers":{"Retry-After":7}}}`,
		}, responseFailure{code: "server_is_overloaded", retryAfter: "7"}},
		{"code without message", []string{
			`{"type":"error","error":{"code":"model_not_found"}}`,
		}, responseFailure{code: "model_not_found"}},
		{"numeric code", []string{
			`{"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED"}}`,
		}, responseFailure{code: "429", message: "Resource has been exhausted."}},
		{"string error", []string{
			`{"error":"upstream rejected the request"}`,
		}, responseFailure{message: "upstream rejected the request"}},
		{"string error beside a flat code", []string{
			`{"type":"error","code":"rate_limit_exceeded","error":"slow down"}`,
		}, responseFailure{code: "rate_limit_exceeded", message: "slow down"}},
		{"null error beside flat fields", []string{
			`{"type":"error","code":"server_error","message":"internal failure","error":null}`,
		}, responseFailure{code: "server_error", message: "internal failure"}},
		{"after visible output", []string{
			delta, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		}, responseFailure{code: "overloaded_error", message: "Overloaded", outputStarted: true}},
		{"later completion is not consumed", []string{
			`{"type":"error","error":{"code":"content_policy_violation","message":"Rejected.","headers":{"retry-after":"300"}}}`, completed,
		}, responseFailure{code: "content_policy_violation", message: "Rejected.", retryAfter: "300"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := completeOnce(t, localProvider(t, sseEvents(tc.events...)))
			failure, ok := errors.AsType[*responseFailure](err)
			if !ok {
				t.Fatalf("error = %T %v, want a provider failure", err, err)
			}
			if *failure != tc.want {
				t.Fatalf("failure = %+v, want %+v", *failure, tc.want)
			}
			if _, transport := errors.AsType[*streamFailure](err); transport {
				t.Fatal("provider rejection is still reported as a dropped connection")
			}
		})
	}

	// Nothing to classify: keep treating the event as a broken stream.
	for _, event := range []string{`{"error":null}`, `{"error":{}}`, `{"type":"error","error":{"message":"  "}}`} {
		err := completeOnce(t, localProvider(t, sseEvents(event)))
		if _, ok := errors.AsType[*streamFailure](err); !ok || !isRecoverableError(err) {
			t.Errorf("%s: error = %T %v, want a retryable stream failure", event, err, err)
		}
	}
}

func TestStreamedErrorEventClassification(t *testing.T) {
	nested := func(detail string) error {
		return completeOnce(t, localProvider(t, sseEvents(`{"type":"error","error":`+detail+`}`)))
	}
	for _, tc := range []struct {
		name, detail                  string
		recoverable, overflow, replay bool
	}{
		{"content policy", `{"code":"content_policy_violation","message":"Rejected.","headers":{"retry-after":"300"}}`, false, false, false},
		{"quota", `{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota.","headers":{"retry-after":"1"}}`, false, false, false},
		{"usage limit", `{"code":"organization_usage_limit_exceeded","message":"Usage limit reached."}`, false, false, false},
		{"invalid request", `{"type":"invalid_request_error","code":null,"message":"Unsupported parameter."}`, false, false, false},
		{"model not found", `{"code":"model_not_found"}`, false, false, false},
		{"budget reached", `{"code":"budget_reached","plan_type":42,"headers":{"retry-after":"300"}}`, false, false, false},
		{"unknown code", `{"code":"unknown_failure","headers":{"retry-after":"7"}}`, false, false, false},
		{"context overflow code", `{"code":"context_length_exceeded","headers":{"retry-after":"300"}}`, true, true, false},
		{"context overflow wording", `{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}`, true, true, false},
		{"rejected thinking signature", "{\"type\":\"invalid_request_error\",\"message\":\"Invalid `signature` in `thinking` block\"}", true, false, true},
		{"rate limit", `{"code":"rate_limit_exceeded","message":"Try again in 35 seconds."}`, true, false, false},
		{"Anthropic overload", `{"type":"overloaded_error","message":"Overloaded"}`, true, false, false},
		{"server error", `{"type":"server_error","code":null,"message":"The server had an error."}`, true, false, false},
		{"numeric unavailable", `{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}`, true, false, false},
		{"numeric bad request", `{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nested(tc.detail)
			if got := isRecoverableError(err); got != tc.recoverable {
				t.Errorf("recoverable = %v, want %v: %v", got, tc.recoverable, err)
			}
			if got := isContextOverflowError(err); got != tc.overflow {
				t.Errorf("overflow = %v, want %v: %v", got, tc.overflow, err)
			}
			if got := isReasoningReplayError(err); got != tc.replay {
				t.Errorf("reasoning replay = %v, want %v: %v", got, tc.replay, err)
			}
		})
	}

	now := time.Now()
	if reason, delay := retryPolicy(nested(`{"code":"rate_limit_exceeded","message":"Try again in 35 seconds.","headers":{"Retry-After":"120"}}`), 0, now); reason != "Provider rate limit" || delay != 120*time.Second {
		t.Errorf("streamed header: %q %v", reason, delay)
	}
	if _, delay := retryPolicy(nested(`{"code":"rate_limit_exceeded","message":"Try again in 35 seconds.","headers":{"Retry-After":"bad"}}`), 0, now); delay != 35*time.Second {
		t.Errorf("invalid header did not fall back to the message: %v", delay)
	}
}

// openai-go returns its JSON decoding error when an error body's "error"
// member is not an object. The status must still classify the failure and
// the provider's text must reach the user.
func TestHTTPErrorBodyWithoutErrorObject(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantMessage string
		recoverable bool
		overflow    bool
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, "unauthorized", false, false},
		{"bad request", http.StatusBadRequest, `{"error":"unsupported parameter: store"}`, "unsupported parameter: store", false, false},
		{"not found", http.StatusNotFound, `{"error":"model 'm' not found"}`, "model 'm' not found", false, false},
		{"rate limited", http.StatusTooManyRequests, `{"error":"rate limit reached"}`, "rate limit reached", true, false},
		{"unavailable", http.StatusServiceUnavailable, `{"error":"upstream unavailable"}`, "upstream unavailable", true, false},
		{"overflow wording", http.StatusBadRequest, `{"error":"prompt is too long: 250000 tokens > 200000 maximum"}`, "prompt is too long: 250000 tokens > 200000 maximum", true, true},
		{"array", http.StatusBadRequest, `{"error":["field required"]}`, `["field required"]`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			respond := func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}
			err := completeOnce(t, localProvider(t, respond))
			apiErr, ok := errors.AsType[*openai.Error](err)
			if !ok {
				t.Fatalf("error = %T %v, want an API error", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Message != tc.wantMessage {
				t.Fatalf("status=%d message=%q", apiErr.StatusCode, apiErr.Message)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error text lost the status or provider message: %v", err)
			}
			if got := isRecoverableError(err); got != tc.recoverable {
				t.Fatalf("recoverable = %v, want %v", got, tc.recoverable)
			}
			if got := isContextOverflowError(err); got != tc.overflow {
				t.Fatalf("overflow = %v, want %v", got, tc.overflow)
			}
			if _, delay := retryPolicy(err, 0, time.Now()); tc.recoverable && !tc.overflow && delay != 7*time.Second {
				t.Fatalf("Retry-After header lost: %v", delay)
			}
			if requests != 1 {
				t.Fatalf("requests = %d", requests)
			}
			if dump := string(apiErr.DumpResponse(true)); !strings.Contains(dump, tc.body) {
				t.Fatalf("response body no longer available for diagnostics: %s", dump)
			}
		})
	}

	// The non-streaming briefing request decodes errors the same way.
	requests := 0
	cfg := localProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	})
	a := &Agent{Config: cfg, Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "run tests"}}}}}
	_, err := a.Recap(t.Context())
	if apiErr, ok := errors.AsType[*openai.Error](err); !ok || apiErr.StatusCode != http.StatusUnauthorized || !strings.Contains(err.Error(), "unauthorized") || requests != 1 {
		t.Fatalf("briefing error = %T %v requests=%d", err, err, requests)
	}
}

func TestBriefingRequestIsBoundedByTimeout(t *testing.T) {
	previous := briefingTimeout
	briefingTimeout = 50 * time.Millisecond
	t.Cleanup(func() { briefingTimeout = previous })

	released := make(chan struct{})
	cfg := localProvider(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	})
	t.Cleanup(func() { close(released) })

	a := &Agent{Config: cfg, Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "run tests"}}}}}
	started := time.Now()
	_, err := a.Recap(t.Context())
	if err == nil || !strings.Contains(err.Error(), "briefing request timed out") {
		t.Fatalf("error = %v", err)
	}
	// A provider stall is a failure, not a cancellation by the user.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("timeout reported as cancellation: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stalled briefing was not bounded: %v", elapsed)
	}

	// The caller's own cancellation keeps its identity.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Recap(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled briefing = %v", err)
	}
}

// Generate shares the Responses client, so a string-valued error body must
// keep its status and text there as well.
func TestGenerateKeepsStatusForStringErrorBody(t *testing.T) {
	cfg := localProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	})
	_, err := cfg.Generate(t.Context(), GenerateOptions{Model: "m", Input: "test"})
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok {
		t.Fatalf("error = %T %v, want an API error", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "unauthorized" {
		t.Fatalf("status=%d message=%q", apiErr.StatusCode, apiErr.Message)
	}
	if isRecoverableError(err) {
		t.Fatal("an unauthorized request must not be retried")
	}
}
