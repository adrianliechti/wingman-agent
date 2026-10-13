package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(fn roundTripFunc) openai.Client {
	return openai.NewClient(option.WithBaseURL("http://gateway.test/v1"), option.WithAPIKey("-"), option.WithHTTPClient(&http.Client{Transport: fn}))
}

type blockedBody struct{ ctx context.Context }

func (b blockedBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b blockedBody) Close() error             { return nil }

func TestModelDiscoveryDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		callerTimeout time.Duration
		body          bool
	}{
		{name: "headers"},
		{name: "body", body: true},
		{name: "caller deadline", callerTimeout: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				want := modelDiscoveryTimeout
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
					want = tc.callerTimeout
				}
				client := testClient(func(r *http.Request) (*http.Response, error) {
					if tc.body {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: blockedBody{r.Context()}, Request: r}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				start := time.Now()
				_, err := ModelIDs(ctx, client)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != want {
					t.Fatalf("discovery took %s, error %v; want %s deadline", time.Since(start), err, want)
				}
			})
		})
	}
}

func TestModelDiscoveryRetriesOnlyTransientErrors(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := testClient(func(r *http.Request) (*http.Response, error) {
					calls++
					code, body := http.StatusOK, `{"object":"list","data":[{"id":"test-model"}]}`
					if calls == 1 {
						code, body = status, `{"error":{"message":"not ready"}}`
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				ids, err := ModelIDs(t.Context(), client)
				if status == http.StatusUnauthorized {
					if err == nil || calls != 1 {
						t.Fatalf("auth failure: %d requests, %v", calls, err)
					}
				} else if err != nil || calls != 2 || len(ids) != 1 || ids[0] != "test-model" {
					t.Fatalf("transient recovery: %d requests, %v, %v", calls, ids, err)
				}
			})
		})
	}
}
