package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_COMPLETION_MODELS=claude-sonnet-5,gpt-5.6-terra
// go test ./pkg/code/agent -run TestLiveCompletion -count=1 -v
//
// The first one or three requests ask the selected model for a known progress
// update in isolation. Present its text without turn metadata to exercise the
// ambiguous-stop path on every deployment. The classifier and all subsequent
// agent requests go through the gateway with their original instructions.
func TestLiveCompletion(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_COMPLETION_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_COMPLETION_MODELS to run real inference")
	}
	backend := os.Getenv("WINGMAN_URL")
	if backend == "" {
		t.Fatal("set WINGMAN_URL explicitly for live completion tests")
	}
	for i, model := range models {
		model = strings.TrimSpace(model)
		t.Run(model, func(t *testing.T) {
			t.Run("automatic_continuation", func(t *testing.T) {
				testLiveCompletion(t, backend, model, model, 1)
			})
			t.Run("bounded_recovery_and_resume", func(t *testing.T) {
				testLiveCompletion(t, backend, model, model, 3)
			})
			if len(models) > 1 {
				switchModel := strings.TrimSpace(models[(i+1)%len(models)])
				if switchModel != model {
					t.Run("model_switch_after_fault_"+switchModel, func(t *testing.T) {
						testLiveCompletion(t, backend, model, switchModel, 3)
					})
				}
			}
		})
	}
}

func testLiveCompletion(t *testing.T, backend, model, resumeModel string, faultRequests int64) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ws := newOptionsTestWorkspace(t)
	const brokenSum = "package calc\n\nfunc Sum(a, b int) int { return a - b }\n"
	const sumTest = "package calc\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) { if got := Sum(2, 3); got != 5 { t.Fatalf(\"got %d, want 5\", got) } }\n"
	for name, content := range map[string]string{
		"go.mod":      "module completion.test\n\ngo 1.24\n",
		"sum.go":      brokenSum,
		"sum_test.go": sumTest,
	} {
		if err := os.WriteFile(filepath.Join(ws.RootPath, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	var requests, decisions, utilityChecks, injected atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	upstream, err := harness.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Telemetry.Shutdown(context.Background()) })
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/decisions") {
			decisions.Add(1)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			var meta struct {
				Model  string
				Stream bool
				Tools  []struct{ Name string }
				Text   struct {
					Format struct {
						Schema struct{ Properties map[string]json.RawMessage }
					}
				}
			}
			if err != nil || json.Unmarshal(body, &meta) != nil {
				http.Error(w, "invalid test request", http.StatusBadRequest)
				return
			}
			for _, tool := range meta.Tools {
				if tool.Name == "finish_turn" {
					t.Error("agent still advertised finish_turn")
				}
			}
			if meta.Stream {
				requests.Add(1)
			} else if _, ok := meta.Text.Format.Schema.Properties["progress"]; ok {
				utilityChecks.Add(1)
			}
			if meta.Stream && injected.Load() < faultRequests {
				if err := writeLiveProgress(w, r, upstream, meta.Model, requests.Load()); err != nil {
					t.Errorf("generating progress fixture with %s: %v", meta.Model, err)
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				injected.Add(1)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("WINGMAN_URL", endpoint.URL)
	t.Setenv("WINGMAN_MODEL", model)
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxTurns = 16
	cfg.DisableWebSearch = true
	cfg.DisableWebFetch = true
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !t.Failed() {
			return
		}
		for _, message := range a.Messages(id) {
			if message.Role != harness.RoleAssistant && !message.Hidden {
				continue
			}
			for _, content := range message.Content {
				if content.Text != "" && len(content.Text) < 1500 {
					t.Logf("role=%s phase=%s hidden=%t text=%q", message.Role, message.Phase, message.Hidden, content.Text)
				}
				if content.ToolCall != nil {
					t.Logf("tool call: %s", content.ToolCall.Name)
				}
			}
		}
	}()
	const task = "Read sum.go and sum_test.go. Fix Sum to add its two arguments. Run go test . using exec with validation:true. Do not change the test. Use tools directly and do not delegate. Report only actual tool results."
	if faultRequests == 3 {
		stream, err := a.Send(ctx, id, []harness.Content{{Text: task}})
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		for _, err := range stream {
			runErr = errors.Join(runErr, err)
		}
		if !errors.Is(runErr, harness.ErrTurnIncomplete) || injected.Load() != faultRequests {
			t.Fatalf("fault outcome=%v injected=%d requests=%d", runErr, injected.Load(), requests.Load())
		}
		for _, event := range a.session(id).harness.StateSnapshot().Events {
			if event.Type == harness.EventToolStarted {
				t.Fatal("fault turn unexpectedly executed a tool")
			}
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		a = New(ws, cfg, nil)
		if err := a.LoadSession(ctx, id); err != nil {
			t.Fatal(err)
		}
		saved := a.session(id).harness.TurnReviews()
		if len(saved) != 1 || saved[0].Outcome != "incomplete" {
			t.Fatalf("restored reviews=%+v", saved)
		}
		if resumeModel != model {
			if err := a.SetModel(ctx, id, resumeModel); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := a.SetMode(ctx, id, code.UnattendedModeID); err != nil {
		t.Fatal(err)
	}
	terminal := make(chan code.TurnEvent, 8)
	manager := code.NewTurnManager(ctx, a, func(event code.TurnEvent) {
		if event.Executed {
			terminal <- event
		}
	})
	defer manager.Close()
	toolStarted, releaseTool := make(chan struct{}), make(chan struct{})
	var steered atomic.Bool
	a.session(id).harness.Hooks.PostToolUse = append(a.session(id).harness.Hooks.PostToolUse,
		func(ctx context.Context, _ tool.ToolCall, _ string) (hook.Outcome, error) {
			if !steered.CompareAndSwap(false, true) {
				return hook.Outcome{}, nil
			}
			snapshot, err := manager.Submit(ctx, id, code.TurnInput{ID: "live-steer", Intent: code.TurnInputSteer, Content: []harness.Content{{Text: "Also include STEER_OK in your final report. Continue the requested fix and test."}}})
			if err != nil || snapshot.State != code.TurnInputSteered {
				t.Errorf("steering=%+v error=%v", snapshot, err)
			}
			close(toolStarted)
			select {
			case <-releaseTool:
			case <-ctx.Done():
			}
			return hook.Outcome{}, nil
		})
	input := task
	if faultRequests == 3 {
		input = "Continue the unfinished task. " + task
	}
	if _, err := manager.Submit(ctx, id, code.TurnInput{ID: "live-resume", Content: []harness.Content{{Text: input}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-toolStarted:
	case event := <-terminal:
		t.Fatalf("resume stopped before running a tool after %d requests: %s %v", requests.Load(), event.State, event.Err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	queued, err := manager.Submit(ctx, id, code.TurnInput{ID: "live-followup", Intent: code.TurnInputFollowUp, Content: []harness.Content{{Text: "Without running any work again, recall the recorded test result and include QUEUE_OK in your answer."}}})
	if err != nil || queued.State != code.TurnInputQueued {
		t.Fatalf("follow-up=%+v error=%v", queued, err)
	}
	close(releaseTool)
	for _, wantID := range []string{"live-resume", "live-followup"} {
		select {
		case event := <-terminal:
			if event.InputID != wantID || event.State != code.TurnInputCompleted || event.Err != nil {
				t.Fatalf("terminal=%+v, want completed %s", event, wantID)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	reviews := a.session(id).harness.TurnReviews()
	workIndex := 0
	if faultRequests == 3 {
		workIndex = 1
	}
	if len(reviews) != workIndex+2 || reviews[workIndex].Validation != "passed" || len(reviews[workIndex+1].Checks) != 0 || len(reviews[workIndex+1].Files) != 0 {
		t.Fatalf("work, validation, or follow-up repeated work: %+v", reviews)
	}
	if got, err := os.ReadFile(filepath.Join(ws.RootPath, "sum_test.go")); err != nil || string(got) != sumTest {
		t.Fatalf("fixture test changed: %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(ws.RootPath, "sum.go")); err != nil || string(got) == brokenSum {
		t.Fatalf("fixture source was not updated: %q, err=%v", got, err)
	}
	tested := false
	for _, check := range reviews[workIndex].Checks {
		tested = tested || check.Outcome == "passed" && strings.Contains(check.Command, "go test")
	}
	if !tested {
		t.Fatalf("missing successful go test in validation checks: %+v", reviews[workIndex].Checks)
	}
	var text strings.Builder
	for _, message := range a.Messages(id) {
		for _, content := range message.Content {
			if message.Role == harness.RoleAssistant {
				text.WriteString(content.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "STEER_OK") || !strings.Contains(text.String(), "QUEUE_OK") {
		t.Fatalf("steering or follow-up missing: %q", text.String())
	}
	want, other := utilityChecks.Load(), decisions.Load()
	if cfg.DecisionModel != "" {
		want, other = other, want
	}
	if want < faultRequests || other != 0 {
		t.Fatalf("decision model=%q: decisions=%d utility checks=%d injected=%d", cfg.DecisionModel, decisions.Load(), utilityChecks.Load(), injected.Load())
	}
	usage := a.Usage(id)
	t.Logf("model=%s requests=%d decisions=%d utility checks=%d validation=%s input=%d cached=%d output=%d", model, requests.Load(), decisions.Load(), utilityChecks.Load(), reviews[workIndex].Validation, usage.InputTokens, usage.CacheReadInputTokens, usage.OutputTokens)
}

func writeLiveProgress(w http.ResponseWriter, r *http.Request, upstream *harness.Config, model string, sequence int64) error {
	const progress = "Now I will run the tests."
	result, err := upstream.Generate(r.Context(), harness.GenerateOptions{
		Model: model, Instructions: "Repeat the supplied sentence verbatim. Output only that sentence.", Input: progress,
	})
	if err != nil {
		return err
	}
	if result.Text != progress {
		return fmt.Errorf("expected %q, received %q", progress, result.Text)
	}
	// Forward only the visible reply and its usage. Reasoning and turn metadata
	// from the isolated prompt do not belong in the coding conversation.
	u := result.Usage
	reply, err := json.Marshal(map[string]any{
		"status": "completed", "model": model,
		"output": []any{map[string]any{"type": "message", "id": fmt.Sprintf("progress-%d", sequence), "role": "assistant", "status": "completed",
			"content": []any{map[string]string{"type": "output_text", "text": result.Text}}}},
		"usage": map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
			"input_tokens_details":  map[string]int64{"cached_tokens": u.CacheReadInputTokens, "cache_write_tokens": u.CacheCreationInputTokens},
			"output_tokens_details": map[string]int64{"reasoning_tokens": u.ReasoningTokens}},
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, err = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", reply)
	return err
}
