package fetch

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestFetchOriginCanonicalization(t *testing.T) {
	for raw, want := range map[string]string{
		"https://EXAMPLE.com/a":         "https://example.com:443",
		"https://example.com.:443/b":    "https://example.com:443",
		"http://example.com":            "http://example.com:80",
		"https://example.com:8443":      "https://example.com:8443",
		"https://bücher.example":        "https://xn--bcher-kva.example:443",
		"http://[::1]":                  "http://[::1]:80",
		"http://[0:0:0:0:0:0:0:1]:8000": "http://[::1]:8000",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := fetchOrigin(u); err != nil || got != want {
			t.Errorf("origin(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestFetchSharesConcurrentOriginApproval(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}, 16), make(chan struct{})
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return true, nil
	}}
	approvals := &hostApprovals{}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			if err := approveHost(t.Context(), elicit, approvals, "https://example.com:443", "https://example.com"); err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	// Hold the first prompt while concurrent requests enter the gate.
	time.Sleep(30 * time.Millisecond)
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("one origin prompted %d times", calls.Load())
	}
}

func TestFetchWaitingApprovalCanCancelIndependently(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		close(started)
		<-release
		return true, nil
	}}
	approvals := &hostApprovals{}
	first := make(chan error, 1)
	go func() { first <- approveHost(t.Context(), elicit, approvals, "origin", "url") }()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := approveHost(ctx, elicit, approvals, "origin", "url")
	close(release)
	if firstErr := <-first; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !approvals.allowed("origin") {
		t.Fatalf("waiting caller disrupted approval: %v", err)
	}
}

func TestFetchCancelledApprovalIsNotRemembered(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		cancel()
		return true, nil
	}}
	approvals := &hostApprovals{}
	if err := approveHost(ctx, elicit, approvals, "origin", "url"); !errors.Is(err, context.Canceled) || approvals.allowed("origin") {
		t.Fatalf("remembered cancelled approval: %v", err)
	}
}
