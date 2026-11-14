package agent

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_STEER_WAIT_MODELS=gpt-5.6-sol,claude-sonnet-5-5
// go test ./pkg/code/agent -run '^TestLiveSteerDuringAgentWait$' -v -count=1
func TestLiveSteerDuringAgentWait(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_STEER_WAIT_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_STEER_WAIT_MODELS to run real inference")
	}
	if os.Getenv("WINGMAN_URL") == "" {
		t.Fatal("set WINGMAN_URL explicitly for live steering tests")
	}
	// The background agent outlasts any reasonable reply, so a fast one means the wait gave way.
	const task = "Start one background agent (agent tool, agent_type general-purpose, background true) with exactly this task: " +
		"\"Run the shell command `sleep 90` with exec and then reply with the single word DONE.\" " +
		"Then wait for it with agent_wait and report its result. Do nothing else."
	const steering = "Stop waiting. Reply with exactly the word PINEAPPLE and nothing else."

	for _, model := range models {
		model = strings.TrimSpace(model)
		t.Run(model, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
			defer cancel()
			ws := newOptionsTestWorkspace(t)
			t.Setenv("WINGMAN_MODEL", model)
			cfg, err := DefaultConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.DisableWebSearch = true
			cfg.DisableWebFetch = true
			a := New(ws, cfg, nil)
			t.Cleanup(func() { _ = a.Close() })
			id, err := a.NewSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.SetMode(ctx, id, code.UnattendedModeID); err != nil {
				t.Fatal(err)
			}

			var steeredAt atomic.Int64
			lead := a.session(id).harness
			lead.Hooks.PreToolUse = append(lead.Hooks.PreToolUse, func(_ context.Context, call tool.ToolCall) (hook.PreToolUseOutcome, error) {
				if call.Name == "agent_wait" && steeredAt.CompareAndSwap(0, -1) {
					time.AfterFunc(2*time.Second, func() {
						steeredAt.Store(time.Now().UnixNano())
						if !lead.QueueInput([]harness.Content{{Text: steering}}) {
							t.Error("running turn rejected steering")
						}
					})
				}
				return hook.PreToolUseOutcome{}, nil
			})

			stream, err := a.Send(ctx, id, []harness.Content{{Text: task}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatalf("stream failed: %v", err)
				}
			}
			at := steeredAt.Load()
			if at <= 0 {
				t.Fatal("the model never called agent_wait, so nothing was steered")
			}
			elapsed := time.Since(time.Unix(0, at))
			reply := ""
			for _, message := range a.Messages(id) {
				if message.Role != harness.RoleAssistant {
					continue
				}
				for _, content := range message.Content {
					if content.Text != "" {
						reply = content.Text
					}
				}
			}
			t.Logf("STEER_WAIT_METRIC model=%s after_steer_ms=%d reply=%q", model, elapsed.Milliseconds(), reply)
			if !strings.Contains(strings.ToUpper(reply), "PINEAPPLE") {
				t.Errorf("steering was not followed: %q", reply)
			}
			if elapsed > 45*time.Second {
				t.Errorf("the reply took %s after steering; agent_wait held the message", elapsed.Round(time.Second))
			}
		})
	}
}
