package harness_test

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

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func compactableAgent(t *testing.T) *harness.Agent {
	t.Helper()
	return &harness.Agent{Config: e2eConfig(t), Messages: []harness.Message{
		{Role: harness.RoleUser, Content: []harness.Content{{Text: "original objective"}}},
		{Role: harness.RoleAssistant, Content: []harness.Content{{Text: strings.Repeat("earlier progress ", 1000)}}},
	}}
}

func contextContains(a *harness.Agent, text string) bool {
	for _, message := range a.StateSnapshot().Context {
		for _, content := range message.Content {
			if strings.Contains(content.Text, text) {
				return true
			}
		}
	}
	return false
}

// A transient failure of the summary request gets the turn's retry policy
// instead of degrading straight to the utility briefing (Codex d3147716e0).
func TestCompactionE2ESummaryRetriesTransientFailure(t *testing.T) {
	var summaries atomic.Int64
	var retriedAt time.Time
	secondRequestAt := make(chan time.Time, 1)
	newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !isCheckpointRequest(req) {
			t.Error("transient summary failure fell back to the utility briefing")
			writeSummary(w, "lossy briefing")
			return
		}
		if summaries.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"server_is_overloaded","message":"busy"}}`)
			return
		}
		select {
		case secondRequestAt <- time.Now():
		default:
		}
		fmt.Fprint(w, completedText("Earlier work preserved"))
	})
	var retries []harness.RetryInfo
	ctx := harness.WithStreamEventHandlers(t.Context(), harness.StreamEventHandlers{Retry: func(info harness.RetryInfo) {
		retriedAt = time.Now()
		retries = append(retries, info)
	}})
	a := compactableAgent(t)
	if _, err := a.Compact(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if summaries.Load() != 2 || !contextContains(a, "Earlier work preserved") || a.StateSnapshot().ContextRevision != 1 {
		t.Fatalf("summaries=%d revision=%d", summaries.Load(), a.StateSnapshot().ContextRevision)
	}
	if len(retries) != 1 || retries[0].Attempt != 1 || retries[0].Reason != "Provider temporarily unavailable" || retries[0].DelayMillis < 1000 {
		t.Fatalf("retries = %+v", retries)
	}
	if waited := (<-secondRequestAt).Sub(retriedAt); waited < time.Duration(retries[0].DelayMillis)*time.Millisecond {
		t.Fatalf("retried after %v, announced %dms", waited, retries[0].DelayMillis)
	}
}

func TestCompactionE2ESummaryRetryPolicyAcrossTransports(t *testing.T) {
	for _, tc := range []struct {
		name     string
		respond  func(http.ResponseWriter)
		minDelay int64
		reason   string
	}{
		{"dropped stream", func(w http.ResponseWriter) {
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{}}\n\n")
		}, 1000, "Connection interrupted"},
		{"provider overload code", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "error", "code": "overloaded_error", "message": "Overloaded"})
		}, 1000, ""},
		{"nested rate limit with header", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "slow down", "headers": map[string]any{"Retry-After": "12"}}})
		}, 12000, "Provider rate limit"},
		{"HTTP rate limit with header", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
		}, 12000, "Provider rate limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if !isCheckpointRequest(req) {
					t.Error("transient summary failure fell back to the utility briefing")
					writeSummary(w, "lossy briefing")
					return
				}
				tc.respond(w)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var retries []harness.RetryInfo
			ctx = harness.WithStreamEventHandlers(ctx, harness.StreamEventHandlers{Retry: func(info harness.RetryInfo) {
				retries = append(retries, info)
				cancel() // Observe the policy without sleeping through its backoff.
			}})
			a := compactableAgent(t)
			_, err := a.Compact(ctx, "")
			if !errors.Is(err, context.Canceled) || len(p.snapshot()) != 1 || a.StateSnapshot().ContextRevision != 0 {
				t.Fatalf("error=%v requests=%d", err, len(p.snapshot()))
			}
			if len(retries) != 1 || retries[0].Attempt != 1 || retries[0].DelayMillis < tc.minDelay || (tc.reason != "" && retries[0].Reason != tc.reason) {
				t.Fatalf("retries = %+v", retries)
			}
		})
	}
}

// The briefing cannot get past an account or policy rejection, so the
// rejection is reported as is rather than replaced by a second failure.
func TestCompactionE2ETerminalSummaryFailureSkipsBriefing(t *testing.T) {
	httpError := func(status int, body any) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
		}
	}
	for _, tc := range []struct {
		name    string
		respond func(http.ResponseWriter)
		want    string
	}{
		{"quota over HTTP", httpError(http.StatusTooManyRequests, map[string]any{"code": "insufficient_quota", "message": "You exceeded your current quota."}), "insufficient_quota"},
		{"quota in band", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "error", "error": map[string]any{"code": "insufficient_quota", "message": "You exceeded your current quota.", "headers": map[string]any{"retry-after": "1"}}})
		}, "insufficient_quota"},
		{"usage limit in band", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "organization_usage_limit_exceeded", "message": "Usage limit reached."}}})
		}, "organization_usage_limit_exceeded"},
		{"unauthorized", httpError(http.StatusUnauthorized, map[string]any{"code": "invalid_api_key", "message": "Incorrect API key provided."}), "Incorrect API key provided."},
		{"unauthorized string body", httpError(http.StatusUnauthorized, "unauthorized"), "unauthorized"},
		{"forbidden", httpError(http.StatusForbidden, map[string]any{"message": "Project does not have access to this model."}), "does not have access"},
		{"content policy over HTTP", httpError(http.StatusBadRequest, map[string]any{"code": "content_policy_violation", "message": "The input was rejected by the content policy."}), "content_policy_violation"},
		{"content policy in band", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "error", "error": map[string]any{"code": "content_policy_violation", "message": "The input was rejected by the content policy."}})
		}, "content_policy_violation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if !isCheckpointRequest(req) {
					t.Error("terminal summary failure fell back to the utility briefing")
					writeSummary(w, "lossy briefing")
					return
				}
				tc.respond(w)
			})
			retries := 0
			ctx := harness.WithStreamEventHandlers(t.Context(), harness.StreamEventHandlers{Retry: func(harness.RetryInfo) { retries++ }})
			a := compactableAgent(t)
			_, err := a.Compact(ctx, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if retries != 0 || len(p.snapshot()) != 1 {
				t.Fatalf("retries=%d requests=%d", retries, len(p.snapshot()))
			}
			if state := a.StateSnapshot(); state.ContextRevision != 0 || len(state.Context) != 2 {
				t.Fatal("failed compaction rewrote history")
			}
		})
	}
}

// An input rejection may be an overflow in wording the agent does not
// recognize, or a history item the transcript omits. Only the bounded
// briefing gets past those, so it stays the fallback.
func TestCompactionE2EInputRejectionStillDegradesToBriefing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		respond       func(http.ResponseWriter)
		wantSummaries int64
	}{
		{"invalid request", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"Request body is larger than this deployment accepts."}}`)
		}, 1},
		{"in-band invalid request", func(w http.ResponseWriter) {
			sseEvent(w, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "Could not process image."}})
		}, 1},
		// Recognized overflow: one trimmed retry, then the briefing.
		{"413 without body", func(w http.ResponseWriter) {
			w.Header().Del("Content-Type")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var summaries, briefings atomic.Int64
			newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if isCheckpointRequest(req) {
					summaries.Add(1)
					tc.respond(w)
					return
				}
				briefings.Add(1)
				writeSummary(w, "Briefing checkpoint")
			})
			retries := 0
			ctx := harness.WithStreamEventHandlers(t.Context(), harness.StreamEventHandlers{Retry: func(harness.RetryInfo) { retries++ }})
			a := compactableAgent(t)
			if _, err := a.Compact(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if summaries.Load() != tc.wantSummaries || briefings.Load() != 1 || retries != 0 {
				t.Fatalf("summaries=%d briefings=%d retries=%d", summaries.Load(), briefings.Load(), retries)
			}
			if !contextContains(a, "Briefing checkpoint") || a.StateSnapshot().ContextRevision != 1 {
				t.Fatal("briefing was not installed as the checkpoint")
			}
		})
	}
}
