package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Opt-in live loop coverage; all tools below are local test fixtures.
// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_LOOP_MODELS=gpt-6-sol,claude-sonnet-5
// go test ./pkg/agent -run '^TestLiveLoop$' -v -count=1
func TestLiveLoop(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_LOOP_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_LOOP_MODELS to run real inference")
	}
	if os.Getenv("WINGMAN_URL") == "" {
		t.Fatal("set WINGMAN_URL explicitly for live loop tests")
	}
	for _, model := range models {
		t.Run(strings.TrimSpace(model), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cfg, err := DefaultConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Model = func() string { return strings.TrimSpace(model) }
			cfg.Effort = func() string { return "low" }
			cfg.MaxTurns = 8
			cfg.ContextWindow = -1 // Compaction has its own live test.
			cfg.Instructions = func() string {
				return "Follow the user's test instructions. Use actual tool results, handle a transient tool error by retrying only that lookup, and preserve the original task when steering arrives. Answer briefly."
			}
			var left, right atomic.Int64
			var steerOnce sync.Once
			a := &Agent{Config: cfg}
			cfg.Tools = func() []tool.Tool {
				return []tool.Tool{{
					Name: "lookup_code", Description: "Look up one of two test codes. Retry the same lookup once if it returns a transient error.",
					Parameters: map[string]any{
						"type": "object", "required": []string{"side"}, "additionalProperties": false,
						"properties": map[string]any{"side": map[string]any{"type": "string", "enum": []string{"left", "right"}}},
					},
					Effect: tool.StaticEffect(tool.EffectReadOnly),
					Execute: func(_ context.Context, args map[string]any) (tool.Result, error) {
						steerOnce.Do(func() {
							if !a.QueueInputWithID([]Content{{Text: "Continue the original lookup task. Also include marker STEER_OK in your final answer."}}, "live-steering") {
								t.Error("live tool round rejected queued steering")
							}
						})
						switch args["side"] {
						case "left":
							left.Add(1)
							return tool.Text("left_code=lime-17"), nil
						case "right":
							if right.Add(1) == 1 {
								return tool.Error("TRANSIENT_LOOKUP_FAILURE: retry lookup_code with side=right once. The left lookup does not need to be repeated."), nil
							}
							return tool.Text("right_code=pear-28"), nil
						default:
							return tool.Error("side must be left or right"), nil
						}
					},
				}}
			}
			if err := runLoopTurn(ctx, a, "Use lookup_code to obtain both the left and right codes. You may look them up in parallel. Retry only a lookup that explicitly reports a transient error. Report both codes, following any additional steering."); err != nil {
				t.Fatal(err)
			}
			checkReply := func(a *Agent) {
				t.Helper()
				reply := lastAssistantText(a.MessagesSnapshot())
				for _, want := range []string{"lime-17", "pear-28", "STEER_OK"} {
					if !strings.Contains(reply, want) {
						t.Errorf("reply omitted %q: %q", want, reply)
					}
				}
			}
			checkReply(a)
			if left.Load() != 1 || right.Load() != 2 {
				t.Fatalf("lookup executions: left=%d right=%d, want 1 and 2", left.Load(), right.Load())
			}
			failures, steers := 0, 0
			for _, message := range a.MessagesSnapshot() {
				if message.InputID == "live-steering" {
					steers++
				}
				for _, content := range message.Content {
					if result := content.ToolResult; result != nil && result.IsError {
						failures++
					}
				}
			}
			if failures != 1 || steers != 1 {
				t.Fatalf("persisted tool failures=%d steering messages=%d, want one each", failures, steers)
			}
			path := filepath.Join(t.TempDir(), "state.json")
			state := a.StateSnapshot()
			if err := state.Save(path); err != nil {
				t.Fatal(err)
			}
			var saved State
			if err := saved.Load(path); err != nil {
				t.Fatal(err)
			}
			restored := &Agent{Config: cfg}
			if err := restored.Restore(saved); err != nil {
				t.Fatal(err)
			}
			if err := runLoopTurn(ctx, restored, "Without calling tools, recall the two codes and the steering marker from the previous turn."); err != nil {
				t.Fatal(err)
			}
			checkReply(restored)
			if left.Load() != 1 || right.Load() != 2 {
				t.Fatal("resume repeated completed tool work")
			}
			usage := restored.UsageSnapshot()
			t.Log(fmt.Sprintf("model=%s tool calls=3 recovered errors=1 steering messages=1 disk resume=passed input=%d output=%d", model, usage.InputTokens, usage.OutputTokens))
		})
	}
}
