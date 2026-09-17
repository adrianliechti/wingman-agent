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

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
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
	cfg, err := harness.DefaultConfig()
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
		}
	}
}
