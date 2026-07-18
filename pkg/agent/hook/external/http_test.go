package external

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestHTTPHookRoundtrip(t *testing.T) {
	var received payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte("remote context"))
	}))
	defer srv.Close()

	cfg := &Config{UserPromptSubmit: []Rule{{URL: srv.URL}}}
	out, err := cfg.PromptHooks(t.TempDir(), nil)[0](context.Background(), "hello")
	if err != nil || out != "remote context" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if received.Event != "user_prompt_submit" || received.Prompt != "hello" {
		t.Fatalf("received = %+v", received)
	}
}

func TestHTTPHookFailureBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy violation", http.StatusForbidden)
	}))
	defer srv.Close()

	cfg := &Config{PreToolUse: []Rule{{URL: srv.URL}}}
	result, err := cfg.PreHooks(t.TempDir(), nil)[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "blocked by pre-tool hook") || !strings.Contains(result, "policy violation") {
		t.Fatalf("result = %q", result)
	}
}
