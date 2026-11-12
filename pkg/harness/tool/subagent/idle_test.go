package subagent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
)

func idleConfig(t *testing.T, limit time.Duration, handler http.HandlerFunc) *harness.Config {
	t.Helper()
	previous := agentIdleTimeout
	agentIdleTimeout = limit
	t.Cleanup(func() { agentIdleTimeout = previous })

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("WINGMAN_URL", server.URL)
	cfg, err := harness.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAgentToolHasNoWallClockDeadline(t *testing.T) {
	if got := Tools(&harness.Config{}, nil, nil)[0].Timeout; got >= 0 {
		t.Fatalf("agent tool timeout = %s; an active agent must not be cut off by elapsed time", got)
	}
}

// A child that keeps streaming runs well past the limit; only silence counts.
func TestActiveAgentOutlivesIdleLimit(t *testing.T) {
	const limit = 250 * time.Millisecond
	const deltas = 40 // 40 x 25ms = 4x the limit

	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint(background), func(t *testing.T) {
			cfg := idleConfig(t, limit, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				for i := range deltas {
					fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":%d,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"a\"}\n\n", i+1)
					flusher.Flush()
					select {
					case <-time.After(25 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":%d,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"%s report\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", deltas+1, strings.Repeat("a", deltas))
			})
			registry := task.NewRegistry()
			defer registry.Close()

			began := time.Now()
			out, err := Tools(cfg, nil, registry)[0].Execute(t.Context(), map[string]any{"description": "work", "prompt": "work", "agent_type": "explore", "background": background})
			if err != nil {
				t.Fatal(err)
			}
			result := out.Content
			if background {
				ev := reviewEvent(t, registry)
				if ev.Status != task.StatusDone {
					t.Fatalf("active background agent ended %s: %q", ev.Status, ev.Result)
				}
				result = ev.Result
			} else if out.IsError {
				t.Fatalf("active agent was aborted: %q", out.Content)
			}
			if !strings.Contains(result, "a report") {
				t.Fatalf("result = %q", result)
			}
			if elapsed := time.Since(began); elapsed < 2*limit {
				t.Fatalf("run took %s; it did not outlive the %s limit", elapsed, limit)
			}
		})
	}
}

// A child that goes quiet is aborted with a named cause, and a synchronous
// caller still receives the follow-up id.
func TestSilentAgentHitsIdleLimit(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint(background), func(t *testing.T) {
			cfg := idleConfig(t, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			registry := task.NewRegistry()
			defer registry.Close()

			out, err := Tools(cfg, nil, registry)[0].Execute(t.Context(), map[string]any{"description": "work", "prompt": "work", "agent_type": "explore", "background": background})
			if err != nil {
				t.Fatal(err)
			}
			result := out.Content
			if background {
				result = reviewEvent(t, registry).Result
			} else if !out.IsError || !strings.Contains(out.Content, registry.List()[0].ID) {
				t.Fatalf("stall lost its failure or follow-up id: %#v", out)
			}
			if !strings.Contains(result, "idle time limit reached") || !strings.Contains(result, "100ms") {
				t.Fatalf("result does not name the idle limit: %q", result)
			}
			if status := registry.List()[0].Status(); status != task.StatusFailed {
				t.Fatalf("task status = %s", status)
			}
		})
	}
}
