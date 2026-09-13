package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func signedCompletion(model string, index int, callTool bool) string {
	output := []any{map[string]any{
		"type": "reasoning", "id": fmt.Sprintf("rs_%d", index),
		"encrypted_content": fmt.Sprintf("%s/signature-%d", model, index),
		"summary":           []any{map[string]any{"type": "summary_text", "text": "remember the observed result"}},
	}}
	if callTool {
		output = append(output, map[string]any{"type": "function_call", "call_id": "read-1", "name": "read", "arguments": "{}", "status": "completed"})
	} else {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "release kiwi-73; tests not run"}}})
	}
	event, _ := json.Marshal(map[string]any{
		"type": "response.completed", "response": map[string]any{
			"output": output, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 100},
		},
	})
	return "data: " + string(event) + "\n\n"
}

func TestContextReasoningE2ESwitchDuringToolRoundAndResume(t *testing.T) {
	for _, models := range [][2]string{{"gpt-5.6-terra", "claude-sonnet-5"}, {"claude-sonnet-5", "gpt-5.6-terra"}} {
		t.Run(models[0], func(t *testing.T) {
			var rounds, executions atomic.Int64
			p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if !req.Stream {
					t.Error("model switch unnecessarily invoked summarization")
					writeSummary(w, "unexpected")
					return
				}
				for _, raw := range requestItems(t, req) {
					var item struct {
						EncryptedContent string `json:"encrypted_content"`
					}
					if err := json.Unmarshal(raw, &item); err != nil {
						t.Error(err)
					}
					if item.EncryptedContent != "" && !strings.HasPrefix(item.EncryptedContent, req.Model+"/") {
						t.Error("foreign signature sent to model")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				n := int(rounds.Add(1))
				fmt.Fprint(w, signedCompletion(req.Model, n, n == 1))
			})
			var selected atomic.Value
			selected.Store(models[0])
			cfg := e2eConfig(t, tool.Tool{Name: "read", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				executions.Add(1)
				selected.Store(models[1]) // Switch while the current tool cycle is active.
				return tool.Text("release kiwi-73; tests not run"), nil
			}})
			cfg.Model = func() string { return selected.Load().(string) }
			cfg.ContextWindow = -1
			dir := t.TempDir()
			journal, err := session.OpenJournal(dir, "switch")
			if err != nil {
				t.Fatal(err)
			}
			a := &agent.Agent{Config: cfg, Recorder: journal}
			run := func(prompt string) {
				t.Helper()
				if err := drain(t, a, prompt); err != nil {
					t.Fatal(err)
				}
			}
			restore := func() {
				t.Helper()
				loaded, err := session.Load(dir, "switch")
				if err != nil {
					t.Fatal(err)
				}
				a = &agent.Agent{Config: cfg, Recorder: journal}
				if err := a.Restore(loaded.State); err != nil {
					t.Fatal(err)
				}
			}
			run("read the release") // Requests 1 + 2, A -> B between call and result.
			run("continue")         // Request 3 preserves B's signature.
			selected.Store(models[0])
			restore()
			run("switch back after resume") // Request 4 removes B's signatures.
			run("continue")                 // Request 5 preserves A's new signature.
			restore()
			run("resume with the same model") // Request 6 preserves signatures from disk.
			cfg.Instructions = func() string { return "changed instructions" }
			restore()
			run("continue after instructions changed") // Request 7 clears old prefix bindings.
			cfg.Tools = func() []tool.Tool { return []tool.Tool{{Name: "read", Description: "new tool schema"}} }
			run("continue after tools changed") // Request 8 clears old tool bindings.
			cfg.Effort = func() string { return "low" }
			cfg.ContextInstructions = func() string { return "new appended guidance" }
			run("continue with new effort") // Request 9 preserves the existing signature.
			requests := p.snapshot()
			if len(requests) != 9 {
				t.Fatalf("requests=%d, want 9", len(requests))
			}
			for i, wantSignatures := range []int{0, 0, 1, 0, 1, 2, 0, 0, 1} {
				got := bytes.Count(requests[i].Input, []byte(`"encrypted_content"`))
				if got != wantSignatures {
					t.Fatalf("request %d signatures=%d, want %d", i+1, got, wantSignatures)
				}
				if i > 0 && (!bytes.Contains(requests[i].Input, []byte(`"function_call_output"`)) || !bytes.Contains(requests[i].Input, []byte("tests not run"))) {
					t.Fatal("switch lost completed tool work")
				}
			}
			if executions.Load() != 1 {
				t.Fatal("switch replayed a tool side effect")
			}
			assertRequestPrefix(t, requests[3], requests[4])
			assertRequestPrefix(t, requests[4], requests[5])
			assertRequestPrefix(t, requests[7], requests[8])
			var canonicalSignatures int
			for _, m := range a.MessagesSnapshot() {
				for _, c := range m.Content {
					if c.Reasoning != nil && c.Reasoning.Content != "" {
						canonicalSignatures++
					}
				}
			}
			if canonicalSignatures != 9 {
				t.Fatalf("canonical signatures=%d", canonicalSignatures)
			}
		})
	}
}

func TestContextModelDownshiftE2ECompactsBeforeSmallerModelRequest(t *testing.T) {
	var rounds, summaries atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			summaries.Add(1)
			writeSummary(w, "Original implementation complete; tests not run.")
			return
		}
		if rounds.Add(1) == 1 {
			fmt.Fprint(w, signedCompletion(req.Model, 1, false))
			return
		}
		if !bytes.Contains(req.Input, []byte("[Previous conversation summary]")) || bytes.Contains(req.Input, []byte(`"encrypted_content"`)) {
			t.Error("smaller model received uncompacted or incompatible history")
		}
		fmt.Fprint(w, completedText("continue verification"))
	})
	cfg := e2eConfig(t)
	model := "gpt-5.6-terra"
	cfg.Model = func() string { return model }
	cfg.ContextWindow = 100_000
	a := &agent.Agent{Config: cfg, Messages: []agent.Message{
		{Role: agent.RoleUser, Content: []agent.Content{{Text: "Original goal: preserve compatibility"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("implementation evidence ", 1500)}}},
	}}
	if err := drain(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	model = "claude-sonnet-5"
	cfg.ContextWindow, cfg.ReserveTokens = 6000, 500
	if err := drain(t, a, "verify using the smaller window"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 3 || requests[1].Stream || requests[2].Model != model || summaries.Load() != 1 {
		t.Fatal("downshift did not compact before its first model request")
	}
}

func TestContextRecoveryE2ERejectedSignatureRetriesOnce(t *testing.T) {
	for _, inBand := range []bool{false, true} {
		for _, persist := range []bool{false, true} {
			t.Run(fmt.Sprintf("sse=%t/persistent=%t", inBand, persist), func(t *testing.T) {
				var calls atomic.Int64
				p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
					if !req.Stream {
						t.Error("signature recovery called summarizer")
						return
					}
					n := calls.Add(1)
					if n == 1 {
						fmt.Fprint(w, signedCompletion(req.Model, 1, false))
						return
					}
					if n == 2 || persist {
						if inBand {
							fmt.Fprint(w, "data: {\"type\":\"error\",\"code\":\"invalid_request_error\",\"message\":\"Invalid `signature` in `thinking` block\"}\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"error":{"code":"invalid_encrypted_content","message":"encrypted content could not be verified"}}`)
						}
						return
					}
					fmt.Fprint(w, completedText("continued"))
				})
				a := &agent.Agent{Config: e2eConfig(t)}
				if err := drain(t, a, "first turn"); err != nil {
					t.Fatal(err)
				}
				err := drain(t, a, "second turn")
				if (err != nil) != persist {
					t.Fatalf("recovery error=%v", err)
				}
				requests := p.snapshot()
				if len(requests) != 3 || !bytes.Contains(requests[1].Input, []byte(`"encrypted_content"`)) || bytes.Contains(requests[2].Input, []byte(`"encrypted_content"`)) {
					t.Fatal("signature repair failed to retry exactly once without opaque reasoning")
				}
				if !bytes.Contains(requests[2].Input, []byte("second turn")) || !bytes.Contains(requests[2].Input, []byte("kiwi-73")) {
					t.Fatal("signature repair lost ordinary conversation content")
				}
			})
		}
	}
}

func TestContextRecoveryE2EInBandOverflow(t *testing.T) {
	var requests, summaries atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			summaries.Add(1)
			writeSummary(w, "Earlier work preserved")
			return
		}
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"prompt is too long\"}}}\n\n")
			return
		}
		fmt.Fprint(w, completedText("continued"))
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow = -1
	a := &agent.Agent{Config: cfg, Messages: []agent.Message{{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("observed progress ", 1000)}}}}}
	if err := drain(t, a, "continue the task"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || summaries.Load() != 1 || !bytes.Contains(p.snapshot()[2].Input, []byte("Earlier work preserved")) {
		t.Fatal("in-band context overflow did not recover through compaction")
	}
}
