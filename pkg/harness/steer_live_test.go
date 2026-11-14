package harness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_STEER_MODELS=gpt-5.6-sol,claude-sonnet-5-5
// go test ./pkg/harness -run '^TestLiveSteerPreempt$' -v -count=1
func TestLiveSteerPreempt(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_STEER_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_STEER_MODELS to run real inference")
	}
	if os.Getenv("WINGMAN_URL") == "" {
		t.Fatal("set WINGMAN_URL explicitly for live steering tests")
	}
	const essay = "Write a detailed explanation, at least 900 words long, of how B-trees stay balanced during insertions and deletions. Do not use tools."
	const steering = "Stop writing. Reply with exactly the word PINEAPPLE and nothing else."

	for _, model := range models {
		model = strings.TrimSpace(model)
		// Steering on the first answer text cuts the response after its reasoning item was committed.
		for name, onText := range map[string]bool{"on_first_output": false, "on_first_answer_text": true} {
			t.Run(model+"/"+name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
				defer cancel()
				cfg, err := ConfigFromEnvironment()
				if err != nil {
					t.Fatal(err)
				}
				cfg.Model = func() string { return model }
				cfg.Effort = func() string { return "medium" }
				cfg.ContextWindow = -1
				cfg.Instructions = func() string { return "You are a helpful assistant. Follow the user's latest message." }
				a := &Agent{Config: cfg}

				stream, err := a.Send(ctx, []Content{{Text: essay}})
				if err != nil {
					t.Fatal(err)
				}
				var steeredAt time.Time
				var beforeSteer, afterSteer strings.Builder
				for message, err := range stream {
					if err != nil {
						t.Fatalf("stream failed after steering=%v: %v", !steeredAt.IsZero(), err)
					}
					if message.Role != RoleAssistant {
						continue
					}
					for _, content := range message.Content {
						if steeredAt.IsZero() {
							beforeSteer.WriteString(content.Text)
							if content.Text != "" || (!onText && content.Reasoning != nil) {
								if !a.QueueInput([]Content{{Text: steering}}) {
									t.Fatal("running turn rejected steering")
								}
								steeredAt = time.Now()
							}
							continue
						}
						afterSteer.WriteString(content.Text)
					}
				}
				if steeredAt.IsZero() {
					t.Fatal("the response produced no output to steer on")
				}
				elapsed := time.Since(steeredAt)
				reply := lastAssistantText(a.MessagesSnapshot())
				t.Logf("STEER_METRIC model=%s case=%s after_steer_ms=%d text_before=%d text_after=%d reply=%q", model, name, elapsed.Milliseconds(), beforeSteer.Len(), afterSteer.Len(), reply)
				if !strings.Contains(strings.ToUpper(reply), "PINEAPPLE") {
					t.Errorf("steering was not followed: %q", reply)
				}
				// Without preemption the whole essay, about 6,000 characters, streams first.
				if afterSteer.Len() > 2500 {
					t.Errorf("response kept streaming %d characters after steering; it was not preempted", afterSteer.Len())
				}
			})
		}
	}
}
