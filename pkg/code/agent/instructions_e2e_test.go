package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func TestProjectInstructionsE2ERefreshAtToolBoundaries(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	path := filepath.Join(ws.RootPath, "AGENTS.md")
	other := filepath.Join(ws.RootPath, "CLAUDE.md")
	for name, content := range map[string]string{path: "RULE_ALPHA", other: "RULE_BETA", filepath.Join(ws.RootPath, "notes.txt"): "observed"} {
		if err := os.WriteFile(name, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var inputs [][]json.RawMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		inputs = append(inputs, req.Input)
		n := len(inputs)
		mu.Unlock()
		var changeErr error
		switch n {
		case 1:
			// A directory at the source path produces a real, portable read failure.
			if changeErr = os.Remove(path); changeErr == nil {
				changeErr = os.Mkdir(path, 0755)
			}
			if changeErr == nil {
				changeErr = os.Remove(other)
			}
		case 2:
			if changeErr = os.Remove(path); changeErr == nil {
				changeErr = os.WriteFile(path, []byte("RULE_GAMMA"), 0644)
			}
		case 3:
			changeErr = os.WriteFile(path, nil, 0644)
		case 4:
			changeErr = os.Remove(path)
		}
		if changeErr != nil {
			t.Error(changeErr)
		}
		var output any = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
		if n < 5 {
			output = map[string]any{"type": "function_call", "status": "completed", "call_id": fmt.Sprintf("read-%d", n), "name": "read", "arguments": `{"file_path":"notes.txt"}`}
		}
		event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{output}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	t.Cleanup(provider.Close)
	t.Setenv("WINGMAN_URL", provider.URL)
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireFinish = func(string) bool { return false }
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := a.Send(t.Context(), id, []harness.Content{{Text: "read notes and continue"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 5 {
		t.Fatalf("requests=%d, want 5", len(inputs))
	}
	wantRules := [][3]bool{{true, true, false}, {true, false, false}, {false, false, true}, {}, {}}
	for i, input := range inputs {
		var current string
		for _, raw := range input {
			var item struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.Unmarshal(raw, &item)
			for _, content := range item.Content {
				if strings.HasPrefix(content.Text, "[Session context]") {
					current = content.Text
				}
			}
		}
		for j, rule := range []string{"RULE_ALPHA", "RULE_BETA", "RULE_GAMMA"} {
			if strings.Contains(current, rule) != wantRules[i][j] {
				t.Errorf("request %d wrong %s guidance: %s", i+1, rule, current)
			}
		}
		if i > 0 {
			if len(input) < len(inputs[i-1]) {
				t.Fatal("history was shortened")
			}
			for j, old := range inputs[i-1] {
				if !bytes.Equal(old, input[j]) {
					t.Fatalf("request %d rewrote cached input %d", i+1, j)
				}
			}
			// Incremental updates must not resend unchanged Environment or Memory blocks.
			if i == 1 || i == 2 {
				if strings.Contains(current, "# Environment\n") || strings.Contains(current, "# Memory\n") {
					t.Fatalf("request %d resent unchanged session context sections:\n%s", i+1, current)
				}
			}
		}
	}
}

func TestPlanToAgentTransitionPreservesPromptCachePrefixE2E(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	target := filepath.Join(ws.RootPath, "service.go")
	if err := os.WriteFile(target, []byte("package service\n\nfunc Ready() bool { return false }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type wireReq struct {
		Instructions json.RawMessage   `json:"instructions"`
		Tools        json.RawMessage   `json:"tools"`
		Input        []json.RawMessage `json:"input"`
	}
	var mu sync.Mutex
	var reqs []wireReq
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		n := len(reqs)
		mu.Unlock()

		var output any
		switch n {
		case 1:
			// Attempt an edit while in Plan mode (must be blocked by guardModeTool).
			output = map[string]any{
				"type": "function_call", "status": "completed", "call_id": "plan-edit-1",
				"name":      "edit",
				"arguments": `{"edits":[{"file_path":"service.go","old_string":"return false","new_string":"return true"}]}`,
			}
		case 2:
			output = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Here is the plan."}}}
		case 3:
			// In Agent mode, read then edit succeeds.
			output = map[string]any{"type": "function_call", "status": "completed", "call_id": "agent-read-1", "name": "read", "arguments": `{"file_path":"service.go"}`}
		case 4:
			output = map[string]any{
				"type": "function_call", "status": "completed", "call_id": "agent-edit-1",
				"name":      "edit",
				"arguments": `{"edits":[{"file_path":"service.go","old_string":"return false","new_string":"return true"}]}`,
			}
		default:
			output = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Implemented."}}}
		}
		event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{output}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	t.Cleanup(provider.Close)
	t.Setenv("WINGMAN_URL", provider.URL)

	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireFinish = func(string) bool { return false }
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: Plan mode.
	if err := a.SetMode(t.Context(), id, "plan"); err != nil {
		t.Fatal(err)
	}
	stream, err := a.Send(t.Context(), id, []harness.Content{{Text: "plan the Ready() fix"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	// File must remain untouched after Plan mode blocked edit.
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "return false") {
		t.Fatalf("Plan mode failed to block edit: %s", string(data))
	}

	// Turn 2: Switch to Agent mode and execute!
	if err := a.SetMode(t.Context(), id, "agent"); err != nil {
		t.Fatal(err)
	}
	stream, err = a.Send(t.Context(), id, []harness.Content{{Text: "go ahead and implement it"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "return true") {
		t.Fatalf("Agent mode did not apply edit: %s", string(data))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 5 {
		t.Fatalf("got %d requests, want 5", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if !bytes.Equal(reqs[0].Instructions, reqs[i].Instructions) {
			t.Fatalf("request %d mutated Instructions across Plan->Agent transition", i+1)
		}
		if !bytes.Equal(reqs[0].Tools, reqs[i].Tools) {
			t.Fatalf("request %d mutated Tools schema across Plan->Agent transition", i+1)
		}
		for j, prevItem := range reqs[i-1].Input {
			if !bytes.Equal(prevItem, reqs[i].Input[j]) {
				t.Fatalf("request %d rewrote prior Input item %d across Plan->Agent transition", i+1, j)
			}
		}
	}
}
