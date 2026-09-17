package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

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
