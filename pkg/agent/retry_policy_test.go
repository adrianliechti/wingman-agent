package agent

import (
	"context"
	"errors"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
