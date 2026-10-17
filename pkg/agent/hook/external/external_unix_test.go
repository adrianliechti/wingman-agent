//go:build !windows

package external

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestCommandOversizedHookResponseDoesNotBypassDenial(t *testing.T) {
	const prefix = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"`
	const suffix = `"}}`
	for _, size := range []int{maxHookOutput, maxHookOutput + 1, 2 * maxHookOutput} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := t.TempDir()
			body := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
			if err := os.WriteFile(filepath.Join(dir, "response.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := configFor("PreToolUse", "Bash", Handler{Type: "command", Command: "cat response.json"})
			outcome, err := cfg.Build(dir, nil).PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell"})
			if err != nil || !outcome.Block {
				t.Fatalf("hook denial was lost: %+v, %v", outcome, err)
			}
			if overflow := strings.Contains(outcome.Reason, "exceeded"); overflow != (size > maxHookOutput) {
				t.Fatalf("incorrect overflow handling at %d bytes", size)
			}
		})
	}
}

func TestLoadCodexHooksJSONShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	data := `{
  "description": "Codex-compatible hooks",
  "hooks": {
    "PreToolUse": [{
      "matcher": "^Bash$",
      "hooks": [{
        "type": "command",
        "command": "python3 /tmp/pre.py",
        "timeout": 10,
        "statusMessage": "checking",
        "additionalContextLimit": 4096
      }]
    }],
    "Stop": [{"hooks": [{"type": "http", "url": "https://example.test/hook"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Description != "Codex-compatible hooks" || cfg.RuleCount() != 2 {
		t.Fatalf("config = %+v", cfg)
	}
	handler := cfg.Hooks.PreToolUse[0].Hooks[0]
	if handler.Type != "command" || handler.Timeout == nil || *handler.Timeout != 10 || handler.AdditionalContextLimit == nil || *handler.AdditionalContextLimit != 4096 {
		t.Fatalf("handler = %+v", handler)
	}
}

func TestLoadRejectsTrailingJSONAndNegativeLimits(t *testing.T) {
	for name, data := range map[string]string{
		"trailing": `{"hooks":{}} {}`,
		"timeout":  `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true","timeout":-1}]}]}}`,
		"context":  `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true","additionalContextLimit":-1}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(name, []byte(data)); err == nil {
				t.Fatal("Parse succeeded")
			}
		})
	}
}

func TestLoadRejectsFormerWingmanFlatShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{"preToolUse":[{"command":"true"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected old format rejection, got %v", err)
	}
}

func TestLoadAcceptsCodexReservedHandlerShapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	data := `{"hooks":{"PreToolUse":[{"hooks":[
  {"type":"command","command":"true","command_windows":"cmd /c exit 0"},
  {"type":"mcp_tool","server":"policy","tool":"check","input":{"strict":true}},
  {"type":"prompt"},
  {"type":"agent"}
]}]}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuleCount() != 4 {
		t.Fatalf("rule count = %d, want 4", cfg.RuleCount())
	}
	handlers := cfg.Hooks.PreToolUse[0].Hooks
	if handlers[0].CommandWindowsSnake != "cmd /c exit 0" || handlers[1].Input["strict"] != true {
		t.Fatalf("handlers = %#v", handlers)
	}
}

func TestCodexMatcherSemantics(t *testing.T) {
	tests := []struct {
		matcher string
		input   []string
		want    bool
	}{
		{"", []string{"Bash"}, true},
		{"*", []string{"Bash"}, true},
		{"Edit|Write", []string{"Write"}, true},
		{"Bash", []string{"BashOutput"}, false},
		{"^Bash$", []string{"Bash"}, true},
		{"mcp__memory__.*", []string{"mcp__memory__write"}, true},
		{"^Write$", []string{"Edit", "Write"}, true},
	}
	for _, test := range tests {
		matcher, err := compileMatcher(test.matcher)
		if err != nil {
			t.Fatal(err)
		}
		if got := matcher.matches(test.input); got != test.want {
			t.Errorf("matches(%q, %q) = %v, want %v", test.matcher, test.input, got, test.want)
		}
	}
}

func TestCodexApplyPatchMatcherAliases(t *testing.T) {
	for _, test := range []struct {
		name string
		want []string
	}{
		{"write", []string{"write", "Write"}},
		{"edit", []string{"edit", "Edit"}},
	} {
		name, _, aliases := toolWire(tool.ToolCall{Name: test.name})
		if name != test.name {
			t.Errorf("toolWire(%q) name = %q", test.name, name)
		}
		for _, matcher := range test.want {
			compiled, err := compileMatcher("^" + matcher + "$")
			if err != nil {
				t.Fatal(err)
			}
			if !compiled.matches(aliases) {
				t.Errorf("toolWire(%q) aliases %q do not match %q", test.name, aliases, matcher)
			}
		}
	}
}

func TestPreToolUseCodexPayloadAndStructuredDeny(t *testing.T) {
	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	command := "cat > " + payloadPath + `; printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"destructive command"}}'`
	cfg := configFor("PreToolUse", "^Bash$", Handler{Type: "command", Command: command})
	ctx := hook.WithRuntime(context.Background(), hook.Runtime{
		SessionID:      "thread-1",
		TurnID:         "turn-1",
		CWD:            dir,
		Model:          "gpt-test",
		PermissionMode: "default",
	})

	outcome, err := cfg.Build(dir, nil).PreToolUse[0](ctx, tool.ToolCall{ID: "call-1", Name: "shell", Args: `{"command":"rm -rf build"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Block || outcome.Reason != "destructive command" {
		t.Fatalf("outcome = %+v", outcome)
	}

	payload := readJSONMap(t, payloadPath)
	wantFields := map[string]any{
		"session_id":      "thread-1",
		"turn_id":         "turn-1",
		"cwd":             dir,
		"hook_event_name": "PreToolUse",
		"model":           "gpt-test",
		"permission_mode": "default",
		"tool_name":       "Bash",
		"tool_use_id":     "call-1",
	}
	for key, want := range wantFields {
		if got := payload[key]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}
	if payload["transcript_path"] != nil {
		t.Fatalf("transcript_path = %#v, want null", payload["transcript_path"])
	}
	input := payload["tool_input"].(map[string]any)
	if input["command"] != "rm -rf build" {
		t.Fatalf("tool_input = %#v", input)
	}
}

func TestPreToolUseRewritePreservesShellInputSchema(t *testing.T) {
	command := `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"echo rewritten","yield_time_ms":1000}}}'`
	cfg := configFor("PreToolUse", "Bash", Handler{Type: "command", Command: command})
	for _, tc := range []struct{ name, key, other string }{
		{"exec", "command", "cmd"},
		{"shell", "command", "cmd"},
		{"exec_command", "cmd", "command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]any{tc.key: "echo original"})
			if err != nil {
				t.Fatal(err)
			}
			call := tool.ToolCall{Name: tc.name, Args: string(args)}
			name, input, _ := toolWire(call)
			if name != "Bash" || input["command"] != "echo original" || input["cmd"] != nil {
				t.Fatalf("hook input = %q %#v", name, input)
			}
			outcome, err := cfg.Build(t.TempDir(), nil).PreToolUse[0](context.Background(), call)
			if err != nil {
				t.Fatal(err)
			}
			var updated map[string]any
			if err := json.Unmarshal(outcome.UpdatedInput, &updated); err != nil {
				t.Fatal(err)
			}
			if updated[tc.key] != "echo rewritten" || updated[tc.other] != nil {
				t.Fatalf("updated input = %#v", updated)
			}
		})
	}
}

func TestStructuredOutputRequiresMatchingCodexEventName(t *testing.T) {
	command := `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse","permissionDecision":"deny","permissionDecisionReason":"wrong event"}}'`
	cfg := configFor("PreToolUse", "Bash", Handler{Type: "command", Command: command})
	outcome, err := cfg.Build(t.TempDir(), nil).PreToolUse[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || outcome.Block {
		t.Fatalf("mismatched output was applied: outcome = %+v, err = %v", outcome, err)
	}
}

func TestCodexOutputValidationFailsOpenForInvalidControlCombinations(t *testing.T) {
	tests := []string{
		`{"continue":false,"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"must not apply"}}`,
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":""}}`,
		`{"decision":"approve"}`,
	}
	for _, stdout := range tests {
		parsed := parseResult("PreToolUse", runResult{exitCode: 0, stdout: stdout})
		if parsed.Block || parsed.Stop || len(parsed.updatedInput) != 0 {
			t.Fatalf("invalid output %s was applied: %+v", stdout, parsed)
		}
	}
}

func TestCodexContinueFalseTakesPrecedenceForStoppableEvents(t *testing.T) {
	for _, test := range []struct {
		event  string
		stdout string
	}{
		{"PostToolUse", `{"continue":false,"suppressOutput":true}`},
		{"UserPromptSubmit", `{"continue":false,"decision":"block"}`},
		{"Stop", `{"continue":false,"decision":"block"}`},
	} {
		parsed := parseResult(test.event, runResult{exitCode: 0, stdout: test.stdout})
		if !parsed.Stop || parsed.Block {
			t.Errorf("%s output was not treated as stop: %+v", test.event, parsed)
		}
	}
}

func TestCodexConcurrentAggregationUsesConfiguredOrderAndLatestCompletedRewrite(t *testing.T) {
	first := Handler{Type: "command", Command: `sleep 0.05; printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"first"},"additionalContext":"first"}}'`}
	second := Handler{Type: "command", Command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"second"},"additionalContext":"second"}}'`}
	cfg := &Config{Hooks: Events{PreToolUse: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{first, second}}}}}
	outcome, err := cfg.Build(t.TempDir(), nil).PreToolUse[0](context.Background(), tool.ToolCall{Name: "shell", Args: `{"command":"original"}`})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(outcome.AdditionalContext, ","); got != "first,second" {
		t.Fatalf("additional context order = %q, want configured order", got)
	}
	var updated map[string]any
	if err := json.Unmarshal(outcome.UpdatedInput, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["command"] != "first" {
		t.Fatalf("updated input = %#v, want last-completed rewrite", updated)
	}
}

func TestAsyncCommandHookWithoutRuntimeIsSkipped(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	cfg := configFor("PreToolUse", "Bash", Handler{
		Type:    "command",
		Command: "printf done > " + marker,
		Async:   true,
	})
	if _, err := cfg.Build(dir, nil).PreToolUse[0](context.Background(), tool.ToolCall{Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("async hook ran without a session runtime: %v", err)
	}
}

func TestAsyncCommandHookDeliversContextWithoutControl(t *testing.T) {
	delivered := make(chan []string, 1)
	async := NewAsync(func(runtime hook.Runtime, parts []string) {
		if runtime.SessionID != "s1" {
			t.Errorf("runtime = %+v, want the scheduling session", runtime)
		}
		delivered <- parts
	})
	defer async.Close()
	output := `{"decision":"block","reason":"ignored","hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"lint passed"}}`
	cfg := &Config{Hooks: Events{PostToolUse: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{{
		Type: "command", Command: "sleep 0.1; printf '%s' '" + output + "'", Async: true,
	}}}}}}
	hooks := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Async: async})
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "s1"})
	started := time.Now()
	outcome, err := hooks.PostToolUse[0](ctx, tool.ToolCall{Name: "shell"}, "ok")
	if err != nil || outcome.Block || len(outcome.AdditionalContext) != 0 {
		t.Fatalf("async hook affected the synchronous outcome: %+v, %v", outcome, err)
	}
	if time.Since(started) > 80*time.Millisecond {
		t.Fatal("event waited for the async hook")
	}
	select {
	case parts := <-delivered:
		if len(parts) != 1 || parts[0] != "lint passed" {
			t.Fatalf("delivered = %#v", parts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("async hook context was not delivered")
	}
}

func TestAsyncCloseCancelsAndWaitsForRunningHooks(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")
	async := NewAsync(func(hook.Runtime, []string) { t.Error("canceled hook delivered context") })
	cfg := configFor("UserPromptSubmit", "", Handler{
		Type:    "command",
		Command: "touch " + started + "; sleep 30; touch " + finished + "; echo late",
		Async:   true,
	})
	hooks := cfg.BuildWithOptions(dir, BuildOptions{Async: async})
	if _, err := hooks.UserPromptSubmit[0](t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async hook did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() {
		async.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel the running hook")
	}
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatalf("hook kept running after Close: %v", err)
	}
	if _, err := hooks.UserPromptSubmit[0](t.Context(), "after close"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionEndAsyncHookRunsSynchronously(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ended")
	async := NewAsync(nil)
	defer async.Close()
	cfg := &Config{Hooks: Events{SessionEnd: []MatcherGroup{{Hooks: []Handler{{
		Type: "command", Command: "printf ended > " + marker, Async: true,
	}}}}}}
	cfg.BuildWithOptions(dir, BuildOptions{Async: async}).SessionEnd[0](t.Context(), "other")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("SessionEnd async hook did not run before returning: %v", err)
	}
}

func TestInterruptHookPayloadIgnoresMatcherAndCapsTimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "payload.json")
	timeout := 60
	cfg := &Config{Hooks: Events{Interrupt: []MatcherGroup{
		{Matcher: "^never$", Hooks: []Handler{{Type: "command", Command: "cat > " + marker}}},
		{Hooks: []Handler{{Type: "command", Command: "sleep 30", Timeout: &timeout}}},
	}}}
	hooks := cfg.Build(dir, nil)
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "s1", TurnID: "t1", Model: "m", PermissionMode: "default"})
	started := time.Now()
	hooks.Interrupt[0](ctx)
	if elapsed := time.Since(started); elapsed > maxSessionEndTimeout*time.Second+3*time.Second {
		t.Fatalf("Interrupt hook ran %s, want the %ds cap", elapsed, maxSessionEndTimeout)
	}
	payload := readJSONMap(t, marker)
	if payload["hook_event_name"] != "Interrupt" || payload["session_id"] != "s1" || payload["turn_id"] != "t1" || payload["permission_mode"] != "default" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestPluginEnvironmentPlaceholdersAreSubstituted(t *testing.T) {
	cfg := configFor("SessionStart", "", Handler{
		Type:    "command",
		Command: `printf '%s' '${CLAUDE_PLUGIN_ROOT}/hook'`,
	})
	hooks := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Environment: map[string]string{
		"CLAUDE_PLUGIN_ROOT": "/plugins/demo",
	}})
	outcome, err := hooks.SessionStart[0](context.Background(), "startup")
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.AdditionalContext) != 1 || outcome.AdditionalContext[0] != "/plugins/demo/hook" {
		t.Fatalf("context = %#v", outcome.AdditionalContext)
	}
}

func TestOversizedHookContextSpillsToFile(t *testing.T) {
	dir := t.TempDir()
	spill := filepath.Join(dir, "spill")
	body := "HEAD" + strings.Repeat("x\n", defaultAdditionalContextTokens*4) + "TAIL"
	if err := os.WriteFile(filepath.Join(dir, "context.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := configFor("UserPromptSubmit", "", Handler{Type: "command", Command: "cat context.txt"})
	outcome, err := cfg.BuildWithOptions(dir, BuildOptions{SpillDir: spill}).UserPromptSubmit[0](t.Context(), "hi")
	if err != nil || len(outcome.AdditionalContext) != 1 {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	preview := outcome.AdditionalContext[0]
	_, path, ok := strings.Cut(preview, "Full output saved to: ")
	if !ok || !strings.HasPrefix(preview, "HEAD") || len(preview) > defaultAdditionalContextTokens*4+len(path)+64 {
		t.Fatalf("preview missing head, path, or budget: %d bytes", len(preview))
	}
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != strings.TrimSpace(body) {
		t.Fatalf("spilled file = %d bytes, %v", len(saved), err)
	}
}

func TestPostToolUseFeedbackSpillsToFile(t *testing.T) {
	dir := t.TempDir()
	reason := strings.Repeat("r", defaultAdditionalContextTokens*8)
	cfg := &Config{Hooks: Events{PostToolUse: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{{
		Type: "command", Command: "printf '" + reason + "' >&2; exit 2",
	}}}}}}
	outcome, err := cfg.BuildWithOptions(dir, BuildOptions{SpillDir: dir}).PostToolUse[0](t.Context(), tool.ToolCall{Name: "shell"}, "ok")
	if err != nil || !outcome.Block || !strings.Contains(outcome.Reason, "Full output saved to: ") {
		t.Fatalf("feedback was not spilled: block=%v len=%d, %v", outcome.Block, len(outcome.Reason), err)
	}
}

func TestCodexExitCodeTwoBlocksButOneFailsOpen(t *testing.T) {
	blocked := configFor("PreToolUse", "Bash", Handler{Type: "command", Command: "echo denied >&2; exit 2"})
	outcome, err := blocked.Build(t.TempDir(), nil).PreToolUse[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || !outcome.Block || outcome.Reason != "denied" {
		t.Fatalf("exit 2 outcome = %+v, err = %v", outcome, err)
	}

	failed := configFor("PreToolUse", "Bash", Handler{Type: "command", Command: "echo ordinary failure >&2; exit 1"})
	outcome, err = failed.Build(t.TempDir(), nil).PreToolUse[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || outcome.Block {
		t.Fatalf("exit 1 outcome = %+v, err = %v", outcome, err)
	}
}

func TestPermissionRequestDenyWins(t *testing.T) {
	allow := Handler{Type: "command", Command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}'`}
	deny := Handler{Type: "command", Command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"policy"}}}'`}
	cfg := &Config{Hooks: Events{PermissionRequest: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{allow, deny}}}}}
	outcome, err := cfg.Build(t.TempDir(), nil).PermissionRequest[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || outcome.Behavior != hook.PermissionDeny || outcome.Message != "policy" {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
}

func TestUserPromptSubmitPlainStdoutIsContext(t *testing.T) {
	cfg := configFor("UserPromptSubmit", "ignored", Handler{Type: "command", Command: "printf 'extra context'"})
	outcome, err := cfg.Build(t.TempDir(), nil).UserPromptSubmit[0](context.Background(), "hello")
	if err != nil || len(outcome.AdditionalContext) != 1 || outcome.AdditionalContext[0] != "extra context" {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
}

func TestSessionStartMatcherAndPayload(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "payload.json")
	cfg := configFor("SessionStart", "startup|resume", Handler{Type: "command", Command: "cat > " + marker})
	hooks := cfg.Build(dir, nil)
	if _, err := hooks.SessionStart[0](context.Background(), "compact"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("compact unexpectedly matched: %v", err)
	}
	if _, err := hooks.SessionStart[0](context.Background(), "resume"); err != nil {
		t.Fatal(err)
	}
	payload := readJSONMap(t, marker)
	if payload["hook_event_name"] != "SessionStart" || payload["source"] != "resume" {
		t.Fatalf("payload = %#v", payload)
	}
	if _, ok := payload["turn_id"]; ok {
		t.Fatalf("SessionStart payload must not contain turn_id: %#v", payload)
	}
}

func TestBuildOptionsInjectPluginEnvironment(t *testing.T) {
	cfg := configFor("SessionStart", "", Handler{
		Type:    "command",
		Command: `printf '%s|%s' "$PLUGIN_ROOT" "$PLUGIN_DATA"`,
	})
	hooks := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Environment: map[string]string{
		"PLUGIN_ROOT": "/plugins/demo",
		"PLUGIN_DATA": "/data/demo",
	}})
	outcome, err := hooks.SessionStart[0](context.Background(), "startup")
	if err != nil {
		t.Fatal(err)
	}
	want := "/plugins/demo|/data/demo"
	if len(outcome.AdditionalContext) != 1 || outcome.AdditionalContext[0] != want {
		t.Fatalf("context = %#v, want %q", outcome.AdditionalContext, want)
	}
}

func configFor(event, matcher string, handler Handler) *Config {
	group := []MatcherGroup{{Matcher: matcher, Hooks: []Handler{handler}}}
	cfg := &Config{}
	switch event {
	case "PreToolUse":
		cfg.Hooks.PreToolUse = group
	case "UserPromptSubmit":
		cfg.Hooks.UserPromptSubmit = group
	case "SessionStart":
		cfg.Hooks.SessionStart = group
	}
	return cfg
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
