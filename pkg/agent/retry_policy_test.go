package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestRetryPolicyHonorsRetryAfterAndAddsBoundedJitter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, value := range []string{"12", now.Add(12 * time.Second).Format(http.TimeFormat)} {
		reason, delay := retryPolicy(&openai.Error{StatusCode: 429, Response: &http.Response{Header: http.Header{"Retry-After": []string{value}}}}, 0, now)
		if reason != "Provider rate limit" || delay < 12*time.Second {
			t.Fatalf("retry = %q %v", reason, delay)
		}
	}
	for i := range 20 {
		_, delay := retryPolicy(&openai.Error{StatusCode: 503}, i, now)
		if delay < time.Second || delay > 30*time.Second {
			t.Fatalf("unbounded backoff %v", delay)
		}
	}
}

func TestRetryPolicyStreamDelayHints(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    time.Duration
	}{
		{"Please try again in 11.054s.", 11054 * time.Millisecond},
		{"TRY AGAIN IN 12345 ms.", 12345 * time.Millisecond},
		{"Try again in 9 seconds.", 9 * time.Second},
		{"Try again in 9 second.", 9 * time.Second},
		{"Try again in 1ms.", 0},
		{"Try again in -5s.", 0},
		{"Try again in 9m.", 0},
		{"Try again in 999999999999999999s.", 0},
	} {
		for _, code := range []string{"rate_limit_exceeded", "slow_down"} {
			err := fmt.Errorf("wrapped: %w", &responseFailure{code: code, message: tc.message})
			reason, delay := retryPolicy(err, 0, time.Now())
			if reason != "Provider rate limit" || (tc.want > 0 && delay != tc.want) || (tc.want == 0 && (delay < time.Second || delay > 2*time.Second)) {
				t.Errorf("%s %q: reason=%q delay=%v", code, tc.message, reason, delay)
			}
		}
	}
	_, delay := retryPolicy(&responseFailure{code: "server_error", message: "Try again in 60s."}, 0, time.Now())
	if delay > 2*time.Second {
		t.Fatalf("unrelated error controlled retry delay: %v", delay)
	}
}
func TestHarnessOwnsHTTPRetriesAndRetryCanBeCancelled(t *testing.T) {
	calls := 0
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)), Request: r}, nil
	})}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	retries := 0
	ctx = WithStreamEventHandlers(ctx, StreamEventHandlers{Retry: func(info RetryInfo) {
		retries++
		if info.DelayMillis < 60000 {
			t.Errorf("ignored retry-after: %+v", info)
		}
		cancel()
	}})
	a := &Agent{Config: &Config{client: &client}}
	stream, err := a.Send(ctx, []Content{{Text: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		runErr = err
	}
	if calls != 1 || retries != 1 || !errors.Is(runErr, context.Canceled) {
		t.Fatalf("requests=%d retries=%d error=%v", calls, retries, runErr)
	}
}
