package external

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestExternalHookDiagnosticsPreserveDecisionsAndIdentifySource(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/invalid":
			fmt.Fprint(w, `{"private-output-marker":true}`)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "private-output-marker")
		}
	}))
	defer server.Close()
	groups := []MatcherGroup{{Hooks: []Handler{
		{Type: "http", URL: server.URL + "/invalid"},
		{Type: "http", URL: server.URL + "/unavailable"},
	}}}
	cfg := &Config{Hooks: Events{PreToolUse: groups, SessionEnd: groups[0:1]}}
	built := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Source: "workspace/hooks.json"})
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "session", TurnID: "turn"})
	out, err := built.PreToolUse[0](ctx, tool.ToolCall{Name: "read"})
	if err != nil || out.Stop || out.Block || len(out.UpdatedInput) != 0 {
		t.Fatalf("diagnostics changed external hook failure policy: %+v, %v", out, err)
	}
	// SessionEnd ignores stdout but still reports execution failures.
	built.SessionEnd[0](ctx, "other")
	if strings.Contains(logs.String(), "private-output-marker") {
		t.Fatalf("diagnostics disclosed hook output: %s", &logs)
	}
	decoder := json.NewDecoder(&logs)
	counts := map[string]int{}
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["source"] != "workspace/hooks.json" || entry["session_id"] != "session" || entry["turn_id"] != "turn" {
			t.Fatalf("missing diagnostic identity: %v", entry)
		}
		counts[entry["event"].(string)]++
	}
	if counts["PreToolUse"] != 2 || counts["SessionEnd"] != 1 {
		t.Fatalf("missing or duplicated hook diagnostics: %v", counts)
	}
}

func TestAsyncHookFailureRetainsSessionCorrelation(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	async := NewAsync(nil)
	defer async.Close()
	cfg := &Config{Hooks: Events{SessionStart: []MatcherGroup{{Hooks: []Handler{
		{Type: "http", URL: server.URL, Async: true},
	}}}}}
	built := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Source: "plugin diagnostics", Async: async})
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "session", TurnID: "turn"})
	if _, err := built.SessionStart[0](ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	async.wg.Wait()
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["session_id"] != "session" || entry["turn_id"] != "turn" || entry["source"] != "plugin diagnostics" {
		t.Fatalf("async failure lost its identity: %v", entry)
	}
}
