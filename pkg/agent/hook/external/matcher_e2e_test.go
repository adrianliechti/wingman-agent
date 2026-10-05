package external

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestCompiledMatcherHTTPDispatchE2E(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		match   string
		miss    string
	}{
		{"", "shell", ""},
		{"*", "shell", ""},
		{"Bash", "shell", "write"},
		{"Bash|Write", "write", "read"},
		{"^Bash$", "shell", "write"},
		{"mcp__memory__.*", "mcp__memory__write", "read"},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["hook_event_name"] != "PreToolUse" {
					t.Errorf("hook payload=%v error=%v", payload, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"matched"}}`))
			}))
			defer server.Close()
			data, err := json.Marshal(Config{Hooks: Events{PreToolUse: []MatcherGroup{{
				Matcher: tc.pattern, Hooks: []Handler{{Type: "http", URL: server.URL}},
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Parse("hooks.json", data)
			if err != nil {
				t.Fatal(err)
			}
			compiled := cfg.Hooks.PreToolUse[0].compiled
			hooks := cfg.Build(t.TempDir(), nil)
			for range 2 {
				outcome, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: tc.match, Args: `{}`})
				if err != nil || !outcome.Block || outcome.Reason != "matched" {
					t.Fatalf("matching dispatch=%+v error=%v", outcome, err)
				}
			}
			if tc.miss != "" {
				outcome, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: tc.miss, Args: `{}`})
				if err != nil || outcome.Block {
					t.Fatalf("nonmatching dispatch=%+v error=%v", outcome, err)
				}
			}
			if calls.Load() != 2 || compiled == nil || cfg.Hooks.PreToolUse[0].compiled != compiled {
				t.Fatalf("calls=%d; compiled matcher was not retained", calls.Load())
			}
		})
	}
}

func TestBuiltMatcherSnapshotSurvivesConfigurationChanges(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg := &Config{Hooks: Events{PreToolUse: []MatcherGroup{{
		Matcher: "^Bash$", Hooks: []Handler{{Type: "http", URL: server.URL}},
	}}}}
	old := cfg.Build(t.TempDir(), nil)
	cfg.Hooks.PreToolUse[0].Matcher = "^Write$"
	updated := cfg.Build(t.TempDir(), nil)
	for _, tc := range []struct {
		old  bool
		name string
	}{
		{true, "shell"}, {true, "write"}, {false, "shell"}, {false, "write"},
	} {
		hooks := updated
		if tc.old {
			hooks = old
		}
		before := calls.Load()
		if _, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: tc.name, Args: `{}`}); err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if (tc.old && tc.name == "shell") || (!tc.old && tc.name == "write") {
			want = 1
		}
		if calls.Load()-before != want {
			t.Fatalf("old=%v tool=%s: hook dispatched unexpectedly", tc.old, tc.name)
		}
	}
}
