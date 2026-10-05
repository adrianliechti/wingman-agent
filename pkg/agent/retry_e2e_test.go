package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
)

func writeProviderFailure(w http.ResponseWriter, transport, code, message string) {
	failure := map[string]any{"code": code, "message": message}
	if transport == "http" {
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusTooManyRequests
		if code == "slow_down" {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": failure})
		return
	}
	var event any = map[string]any{"type": "error", "code": code, "message": message}
	if transport == "response.failed" {
		event = map[string]any{"type": "response.failed", "response": map[string]any{"error": failure}}
	}
	encoded, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
}

func TestRetryE2EStreamedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		headers    any
		wantDelay  int64
		terminal   bool
	}{
		{"header overrides message", "rate_limit_exceeded", map[string]any{"Retry-After": "12"}, 12000, false},
		{"lowercase numeric header", "slow_down", map[string]any{"retry-after": 12}, 12000, false},
		{"server error", "server_error", map[string]any{"Retry-After": "12"}, 12000, false},
		{"overloaded", "server_is_overloaded", map[string]any{"Retry-After": "12"}, 12000, false},
		{"invalid header falls back", "rate_limit_exceeded", map[string]any{"Retry-After": "invalid"}, 9000, false},
		{"invalid header bytes", "rate_limit_exceeded", map[string]any{"Retry-After": "12\r\n"}, 9000, false},
		{"unsupported header value", "rate_limit_exceeded", map[string]any{"Retry-After": []string{"12"}}, 9000, false},
		{"overflow header falls back", "rate_limit_exceeded", map[string]any{"Retry-After": "9223372036854775807"}, 9000, false},
		{"malformed headers fall back", "rate_limit_exceeded", "invalid", 9000, false},
		{"quota stays terminal", "insufficient_quota", map[string]any{"Retry-After": "12"}, 0, true},
		{"invalid request stays terminal", "invalid_prompt", map[string]any{"Retry-After": "12"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
				event := map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{
					"code": tc.code, "message": "Please try again in 9 seconds.", "headers": tc.headers,
				}}}
				encoded, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\n", encoded)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var retries []agent.RetryInfo
			ctx = agent.WithStreamEventHandlers(ctx, agent.StreamEventHandlers{Retry: func(info agent.RetryInfo) {
				retries = append(retries, info)
				cancel() // Exercise policy and cancellation without waiting for the advertised delay.
			}})
			a := &agent.Agent{Config: e2eConfig(t)}
			stream, err := a.Send(ctx, []agent.Content{{Text: "work"}})
			if err != nil {
				t.Fatal(err)
			}
			var runErr error
			for _, err := range stream {
				runErr = err
			}
			if len(p.snapshot()) != 1 {
				t.Fatalf("requests = %d", len(p.snapshot()))
			}
			if tc.terminal {
				if len(retries) != 0 || runErr == nil || !strings.Contains(runErr.Error(), tc.code) {
					t.Fatalf("terminal error=%v retries=%+v", runErr, retries)
				}
			} else if len(retries) != 1 || retries[0].DelayMillis != tc.wantDelay || !errors.Is(runErr, context.Canceled) {
				t.Fatalf("error=%v retries=%+v want delay=%d", runErr, retries, tc.wantDelay)
			}
		})
	}
}

func TestRetryE2EStreamedOverloadWaitsAndRecovers(t *testing.T) {
	var requests atomic.Int64
	var retriedAt time.Time
	var advertisedDelay int64
	secondRequestAt := make(chan time.Time, 1)
	p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"busy\",\"headers\":{\"Retry-After\":\"2\"}}}}\n\n")
			return
		}
		select {
		case secondRequestAt <- time.Now():
		default:
		}
		fmt.Fprint(w, completedText("recovered"))
	})
	ctx := agent.WithStreamEventHandlers(t.Context(), agent.StreamEventHandlers{Retry: func(info agent.RetryInfo) {
		retriedAt = time.Now()
		advertisedDelay = info.DelayMillis
	}})
	a := &agent.Agent{Config: e2eConfig(t)}
	stream, err := a.Send(ctx, []agent.Content{{Text: "preserve this input"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	captured := p.snapshot()
	if len(captured) != 2 || string(captured[0].Input) != string(captured[1].Input) {
		t.Fatalf("retry changed input: %+v", captured)
	}
	elapsed := (<-secondRequestAt).Sub(retriedAt)
	if advertisedDelay != 2000 || elapsed < 2*time.Second {
		t.Fatalf("retry ignored delay: advertised=%d elapsed=%v", advertisedDelay, elapsed)
	}
	messages := a.MessagesSnapshot()
	if len(messages) != 2 || messages[1].Content[0].Text != "recovered" {
		t.Fatalf("recovered history = %+v", messages)
	}
}

func TestRetryE2EQuotaTerminatesWithoutRetry(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded"} {
		for _, transport := range []string{"http", "error", "response.failed"} {
			t.Run(code+"/"+transport, func(t *testing.T) {
				p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
					writeProviderFailure(w, transport, code, "Account limit reached")
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				retries := 0
				ctx = agent.WithStreamEventHandlers(ctx, agent.StreamEventHandlers{Retry: func(agent.RetryInfo) {
					retries++
					cancel() // A regression must fail immediately, not sleep through backoff.
				}})
				a := &agent.Agent{Config: e2eConfig(t)}
				stream, err := a.Send(ctx, []agent.Content{{Text: "work"}})
				if err != nil {
					t.Fatal(err)
				}
				var runErr error
				for _, err := range stream {
					runErr = err
				}
				if runErr == nil || !strings.Contains(runErr.Error(), code) || retries != 0 || len(p.snapshot()) != 1 {
					t.Fatalf("error=%v retries=%d requests=%d", runErr, retries, len(p.snapshot()))
				}
			})
		}
	}
}

func TestRetryE2ESlowDownRecoversWithSameInput(t *testing.T) {
	for _, transport := range []string{"http", "error", "response.failed"} {
		t.Run(transport, func(t *testing.T) {
			var requests atomic.Int64
			p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
				if requests.Add(1) == 1 {
					writeProviderFailure(w, transport, "slow_down", "Please try again in 2.001 seconds.")
					return
				}
				fmt.Fprint(w, completedText("recovered"))
			})
			var retries []agent.RetryInfo
			ctx := agent.WithStreamEventHandlers(t.Context(), agent.StreamEventHandlers{Retry: func(info agent.RetryInfo) {
				retries = append(retries, info)
			}})
			a := &agent.Agent{Config: e2eConfig(t)}
			stream, err := a.Send(ctx, []agent.Content{{Text: "preserve this input"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(retries) != 1 || retries[0] != (agent.RetryInfo{Attempt: 1, Reason: "Provider rate limit", DelayMillis: 2001}) {
				t.Fatalf("retries = %+v", retries)
			}
			captured := p.snapshot()
			if len(captured) != 2 || string(captured[0].Input) != string(captured[1].Input) {
				t.Fatalf("retry changed the submitted input: %+v", captured)
			}
			messages := a.MessagesSnapshot()
			if len(messages) != 2 || messages[1].Content[0].Text != "recovered" {
				t.Fatalf("recovered history = %+v", messages)
			}
		})
	}
}
