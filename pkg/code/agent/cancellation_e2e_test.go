package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func cancellationE2EAgent(t *testing.T, hooks hook.Hooks) (*Agent, string, *atomic.Int64) {
	t.Helper()
	ws := newOptionsTestWorkspace(t)
	requests := &atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}]}}\n\n")
	}))
	t.Cleanup(backend.Close)
	t.Setenv("WINGMAN_URL", backend.URL)
	cfg, err := harness.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = func() string { return "test-model" }
	cfg.RequireFinish = func(string) bool { return false }
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Derive intentionally excludes main-session hooks for delegated agents;
	// install fixture hooks on this top-level session before its first Send.
	a.session(id).aa.Hooks.Append(hooks)
	return a, id, requests
}

func TestSessionStartupCancellationE2E(t *testing.T) {
	for _, phase := range []string{"session start", "user prompt"} {
		t.Run(phase, func(t *testing.T) {
			for _, action := range []string{"cancel", "close"} {
				t.Run(action, func(t *testing.T) {
					entered := make(chan struct{})
					var calls atomic.Int64
					block := func(ctx context.Context, _ string) (hook.Outcome, error) {
						if calls.Add(1) == 1 {
							close(entered)
							<-ctx.Done()
							return hook.Outcome{}, ctx.Err()
						}
						return hook.Outcome{}, nil
					}
					var hooks hook.Hooks
					if phase == "session start" {
						hooks.SessionStart = []hook.SessionStart{block}
					} else {
						hooks.UserPromptSubmit = []hook.UserPromptSubmit{block}
					}
					a, id, requests := cancellationE2EAgent(t, hooks)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					finished := make(chan error, 1)
					go func() {
						stream, err := a.Send(ctx, id, []harness.Content{{Text: "cancel during startup"}})
						if err == nil {
							for _, streamErr := range stream {
								err = errors.Join(err, streamErr)
							}
						}
						finished <- err
					}()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("startup hook was not reached")
					}
					cancelReturned := make(chan struct{})
					go func() {
						if action == "close" {
							_ = a.Close()
						} else {
							a.Cancel(id)
						}
						close(cancelReturned)
					}()
					select {
					case <-cancelReturned:
					case <-time.After(time.Second):
						cancel() // Unblock setup before reporting a regression.
						<-finished
						<-cancelReturned
						t.Fatal("cancellation blocked behind the startup hook")
					}
					if err := <-finished; !errors.Is(err, context.Canceled) {
						t.Fatalf("setup outcome = %v", err)
					}
					if requests.Load() != 0 {
						t.Fatalf("cancelled setup sent %d model requests", requests.Load())
					}
					if action == "cancel" {
						if len(a.Messages(id)) != 0 {
							t.Fatal("cancelled setup persisted its input")
						}
						stream, err := a.Send(ctx, id, []harness.Content{{Text: "next turn"}})
						if err != nil {
							t.Fatalf("next turn did not release setup reservation: %v", err)
						}
						var visible strings.Builder
						for msg, err := range stream {
							if err != nil {
								t.Fatal(err)
							}
							for _, c := range msg.Content {
								visible.WriteString(c.AsText())
							}
						}
						if visible.String() != "done" || requests.Load() != 1 {
							t.Fatalf("next turn visible=%q requests=%d", visible.String(), requests.Load())
						}
					}
				})
			}
		})
	}
}

func TestConcurrentSessionStartupSendE2E(t *testing.T) {
	entered := make(chan struct{})
	a, id, requests := cancellationE2EAgent(t, hook.Hooks{SessionStart: []hook.SessionStart{
		func(ctx context.Context, _ string) (hook.Outcome, error) {
			close(entered)
			<-ctx.Done()
			return hook.Outcome{}, ctx.Err()
		},
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	send := func(text string, result chan<- error) {
		stream, err := a.Send(ctx, id, []harness.Content{{Text: text}})
		if err == nil {
			for _, streamErr := range stream {
				err = errors.Join(err, streamErr)
			}
		}
		result <- err
	}
	first := make(chan error, 1)
	go send("first", first)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("startup hook was not reached")
	}
	second := make(chan error, 1)
	go send("overlap", second)
	select {
	case err := <-second:
		if !errors.Is(err, code.ErrTurnInProgress) {
			t.Errorf("concurrent send = %v", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-second
		t.Error("concurrent send blocked behind startup")
	}
	a.Cancel(id)
	if err := <-first; !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("first turn error=%v requests=%d", err, requests.Load())
	}
}

func TestAlreadyCancelledSessionSendE2E(t *testing.T) {
	var starts, prompts atomic.Int64
	a, id, requests := cancellationE2EAgent(t, hook.Hooks{
		SessionStart: []hook.SessionStart{func(context.Context, string) (hook.Outcome, error) {
			starts.Add(1)
			return hook.Outcome{}, nil
		}},
		UserPromptSubmit: []hook.UserPromptSubmit{func(context.Context, string) (hook.Outcome, error) {
			prompts.Add(1)
			return hook.Outcome{}, nil
		}},
	})
	before := a.HistorySnapshot(id)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream, err := a.Send(ctx, id, []harness.Content{{Text: "already cancelled"}})
	if !errors.Is(err, context.Canceled) || stream != nil || starts.Load() != 0 || prompts.Load() != 0 || requests.Load() != 0 || !reflect.DeepEqual(before, a.HistorySnapshot(id)) {
		t.Fatalf("error=%v starts=%d prompts=%d requests=%d messages=%d", err, starts.Load(), prompts.Load(), requests.Load(), len(a.Messages(id)))
	}
	// The rejected attempt must not consume the session's one-time startup.
	stream, err = a.Send(t.Context(), id, []harness.Content{{Text: "accepted"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 || prompts.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("starts=%d prompts=%d requests=%d", starts.Load(), prompts.Load(), requests.Load())
	}
	if err := a.Save(id); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := New(a.workspace, a.cfg, nil)
	defer reopened.Close()
	if err := reopened.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, message := range reopened.Messages(id) {
		for _, content := range message.Content {
			if strings.Contains(content.AsText(), "already cancelled") {
				t.Fatal("cancelled input survived journal reload")
			}
		}
	}
}
