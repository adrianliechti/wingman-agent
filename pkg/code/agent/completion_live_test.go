package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_COMPLETION_MODELS=claude-sonnet-5,gpt-5.6-terra
// go test ./pkg/code/agent -run TestLiveCompletion -count=1 -v
//
// All inference goes to the real backend. The first one or three responses
// inject a protocol fault: tool_choice:none and progress-only instructions. For models
// using phases, those fault responses are labelled commentary at the proxy.
// Reasoning from the injected prompt is discarded: it does not belong to the
// agent's original instructions and must not be replayed after the fault.
// After that fault, requests and streamed responses pass through unchanged.
// Real read/edit/exec tools then fix and test a Go fixture, with steering, a FIFO
// follow-up, and restoration from disk through the production coding agent.
func TestLiveCompletion(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_COMPLETION_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_COMPLETION_MODELS to run real inference")
	}
	backend := os.Getenv("WINGMAN_URL")
	if backend == "" {
		t.Fatal("set WINGMAN_URL explicitly for live completion tests")
	}
	for _, model := range models {
		t.Run(strings.TrimSpace(model), func(t *testing.T) {
			t.Run("automatic_continuation", func(t *testing.T) {
				testLiveCompletion(t, backend, strings.TrimSpace(model), 1)
			})
			t.Run("bounded_recovery_and_resume", func(t *testing.T) {
				testLiveCompletion(t, backend, strings.TrimSpace(model), 3)
			})
		})
	}
}

type liveProgressKey struct{}

func testLiveCompletion(t *testing.T, backend, model string, faultRequests int64) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ws := newOptionsTestWorkspace(t)
	for name, content := range map[string]string{
		"go.mod":      "module completion.test\n\ngo 1.24\n",
		"sum.go":      "package calc\n\nfunc Sum(a, b int) int { return a - b }\n",
		"sum_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) { if got := Sum(2, 3); got != 5 { t.Fatalf(\"got %d, want 5\", got) } }\n",
	} {
		if err := os.WriteFile(filepath.Join(ws.RootPath, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(resp *http.Response) error {
		fault, _ := resp.Request.Context().Value(liveProgressKey{}).(bool)
		if !fault || resp.StatusCode != http.StatusOK {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		body, err = liveProgressFault(body, !requiresFinish(model))
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
			if requests.Add(1) <= faultRequests {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "invalid test request", http.StatusBadRequest)
					return
				}
				r.Body.Close()
				request["tool_choice"] = "none"
				request["instructions"] = "This is a transport fault test. Reply only with: Now I will run the tests. Do not use tools."
				body, err := json.Marshal(request)
				if err != nil {
					t.Error(err)
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), liveProgressKey{}, true))
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("WINGMAN_URL", endpoint.URL)
	t.Setenv("WINGMAN_MODEL", model)
	cfg, err := harness.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxTurns = 16
	a := New(ws, cfg, nil, Options{DisableWebSearch: true, DisableWebFetch: true})
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
					if content.ToolCall.Name == "finish_turn" {
						t.Logf("finish arguments: %s", content.ToolCall.Args)
					}
				}
				if content.ToolResult != nil && content.ToolResult.Name == "finish_turn" {
					t.Logf("finish result: %s", content.ToolResult.Content)
				}
			}
		}
	}()
	const task = "Read sum.go and sum_test.go. Fix Sum to add its two arguments. Run go test . using exec_command with validation:true. Do not change the test. Use tools directly and do not delegate. Report only actual tool results."
	if faultRequests == 3 {
		stream, err := a.Send(ctx, id, []harness.Content{{Text: task}})
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		for _, err := range stream {
			runErr = errors.Join(runErr, err)
		}
		if !errors.Is(runErr, harness.ErrTurnIncomplete) || requests.Load() != 3 {
			t.Fatalf("fault outcome=%v requests=%d", runErr, requests.Load())
		}
		for _, event := range a.session(id).aa.StateSnapshot().Events {
			if event.Type == harness.EventToolStarted {
				t.Fatal("fault turn unexpectedly executed a tool")
			}
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		a = New(ws, cfg, nil, Options{DisableWebSearch: true, DisableWebFetch: true})
		if err := a.LoadSession(ctx, id); err != nil {
			t.Fatal(err)
		}
		saved := a.session(id).aa.TurnReviews()
		if len(saved) != 1 || saved[0].Outcome != "incomplete" {
			t.Fatalf("restored reviews=%+v", saved)
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
	a.session(id).aa.Hooks.PostToolUse = append(a.session(id).aa.Hooks.PostToolUse,
		func(ctx context.Context, call tool.ToolCall, _ string) (hook.Outcome, error) {
			if call.Name != "read" || !steered.CompareAndSwap(false, true) {
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
		t.Fatalf("resume stopped before reading the fixture: %s %v", event.State, event.Err)
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
	reviews := a.session(id).aa.TurnReviews()
	workIndex := 0
	if faultRequests == 3 {
		workIndex = 1
	}
	if len(reviews) != workIndex+2 || reviews[workIndex].Validation != "passed" || len(reviews[workIndex].Files) == 0 || len(reviews[workIndex+1].Checks) != 0 || len(reviews[workIndex+1].Files) != 0 {
		t.Fatalf("work, validation, or follow-up repeated work: %+v", reviews)
	}
	var text strings.Builder
	finishes := 0
	for _, message := range a.Messages(id) {
		for _, content := range message.Content {
			if message.Role == harness.RoleAssistant {
				text.WriteString(content.Text)
			}
			if result := content.ToolResult; result != nil && result.Name == "finish_turn" && !result.IsError {
				finishes++
			}
		}
	}
	if !strings.Contains(text.String(), "STEER_OK") || !strings.Contains(text.String(), "QUEUE_OK") || (requiresFinish(model) && finishes < 2) {
		t.Fatalf("steering/follow-up/finish missing: accepted finishes=%d text=%q", finishes, text.String())
	}
	usage := a.Usage(id)
	t.Logf("model=%s requests=%d accepted_finishes=%d validation=%s input=%d cached=%d output=%d; progress recovery, real edit/test, steering, FIFO follow-up passed", model, requests.Load(), finishes, reviews[workIndex].Validation, usage.InputTokens, usage.CacheReadInputTokens, usage.OutputTokens)
}

func liveProgressFault(body []byte, commentary bool) ([]byte, error) {
	var mark func(any)
	mark = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if commentary && value["type"] == "message" && value["role"] == "assistant" {
				value["phase"] = "commentary"
			}
			if output, ok := value["output"].([]any); ok {
				kept := output[:0]
				for _, item := range output {
					if object, ok := item.(map[string]any); ok && object["type"] == "reasoning" {
						continue
					}
					kept = append(kept, item)
				}
				value["output"] = kept
			}
			for _, child := range value {
				mark(child)
			}
		case []any:
			for _, child := range value {
				mark(child)
			}
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		typ, _ := event["type"].(string)
		item, _ := event["item"].(map[string]any)
		if strings.Contains(typ, "reasoning") || item["type"] == "reasoning" {
			lines[i] = nil
			continue
		}
		mark(event)
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		lines[i] = append([]byte("data: "), encoded...)
	}
	return bytes.Join(lines, []byte("\n")), nil
}
