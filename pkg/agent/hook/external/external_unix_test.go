//go:build !windows

package external

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestLoadMergesConfigs(t *testing.T) {
	dir := t.TempDir()

	global := filepath.Join(dir, "global.json")
	os.WriteFile(global, []byte(`{"preToolUse":[{"matcher":"shell","command":"true"}]}`), 0644)

	local := filepath.Join(dir, "local.json")
	os.WriteFile(local, []byte(`{"postToolUse":[{"command":"echo hi"}]}`), 0644)

	cfg, err := Load(global, local, filepath.Join(dir, "missing.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PreToolUse) != 1 || len(cfg.PostToolUse) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestPreHookBlocksOnFailure(t *testing.T) {
	cfg := &Config{PreToolUse: []Rule{{Matcher: "shell", Command: "echo not allowed; exit 1"}}}

	hooks := cfg.PreHooks(t.TempDir(), nil)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}

	result, err := hooks[0](context.Background(), tool.ToolCall{Name: "shell", Args: `{"command":"ls"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "blocked by pre-tool hook") || !strings.Contains(result, "not allowed") {
		t.Fatalf("result = %q", result)
	}

	result, err = hooks[0](context.Background(), tool.ToolCall{Name: "read", Args: `{}`})
	if err != nil || result != "" {
		t.Fatalf("non-matching tool blocked: %q, %v", result, err)
	}
}

func TestPreHookAllowsOnSuccess(t *testing.T) {
	cfg := &Config{PreToolUse: []Rule{{Command: "exit 0"}}}

	result, err := cfg.PreHooks(t.TempDir(), nil)[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || result != "" {
		t.Fatalf("passing hook blocked: %q, %v", result, err)
	}
}

func TestPostHookAppendsOutput(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "echo reviewed"}}}

	result, err := cfg.PostHooks(t.TempDir(), nil)[0](context.Background(), tool.ToolCall{Name: "shell"}, "original")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result, "original") || !strings.Contains(result, "<hook-output>\nreviewed\n</hook-output>") {
		t.Fatalf("result = %q", result)
	}
}

func TestPostHookKeepsResultOnFailure(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "echo broken; exit 1"}}}

	result, err := cfg.PostHooks(t.TempDir(), nil)[0](context.Background(), tool.ToolCall{Name: "shell"}, "original")
	if err != nil || result != "original" {
		t.Fatalf("result = %q, %v", result, err)
	}
}

func TestPostHookReceivesPayload(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "cat"}}}

	result, err := cfg.PostHooks(t.TempDir(), nil)[0](context.Background(), tool.ToolCall{Name: "shell", Args: `{"command":"ls"}`}, "out")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"tool_name":"shell"`) || !strings.Contains(result, `"result":"out"`) {
		t.Fatalf("result = %q", result)
	}
}

func TestPromptHookInjectsAndBlocks(t *testing.T) {
	cfg := &Config{UserPromptSubmit: []Rule{{Command: "echo extra context"}}}
	hooks := cfg.PromptHooks(t.TempDir(), nil)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}

	out, err := hooks[0](context.Background(), "do things")
	if err != nil || out != "extra context" {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	blocking := &Config{UserPromptSubmit: []Rule{{Command: "echo nope; exit 1"}}}
	if _, err := blocking.PromptHooks(t.TempDir(), nil)[0](context.Background(), "do things"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected blocking error, got %v", err)
	}
}

func TestPromptHookReceivesPayload(t *testing.T) {
	cfg := &Config{UserPromptSubmit: []Rule{{Command: "cat"}}}
	out, err := cfg.PromptHooks(t.TempDir(), nil)[0](context.Background(), "the prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"event":"user_prompt_submit"`) || !strings.Contains(out, `"prompt":"the prompt"`) {
		t.Fatalf("payload = %q", out)
	}
}

func TestOnceRuleFiresOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "count")
	cfg := &Config{SessionStart: []Rule{{Command: "echo x >> " + marker + "; echo ran", Once: true}}}
	hooks := cfg.StartHooks(t.TempDir(), nil)

	out, err := hooks[0](context.Background())
	if err != nil || out != "ran" {
		t.Fatalf("first fire: out = %q, err = %v", out, err)
	}
	out, err = hooks[0](context.Background())
	if err != nil || out != "" {
		t.Fatalf("second fire: out = %q, err = %v", out, err)
	}

	data, _ := os.ReadFile(marker)
	if strings.Count(string(data), "x") != 1 {
		t.Fatalf("command ran %d times", strings.Count(string(data), "x"))
	}
}

func TestCompactHookBlocks(t *testing.T) {
	cfg := &Config{PreCompact: []Rule{{Command: "exit 1"}}}
	if err := cfg.CompactHooks(t.TempDir(), nil)[0](context.Background()); err == nil {
		t.Fatal("expected block error")
	}

	allow := &Config{PreCompact: []Rule{{Command: "true"}}}
	if err := allow.CompactHooks(t.TempDir(), nil)[0](context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSubagentHookMatchesType(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fired")
	cfg := &Config{SubagentStop: []Rule{{Matcher: "explore", Command: "cat > " + marker}}}
	hooks := cfg.SubagentHooks(t.TempDir(), nil)

	hooks[0](context.Background(), "general-purpose", "result")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook fired for non-matching agent type")
	}

	hooks[0](context.Background(), "explore", "result text")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"agent_type":"explore"`) || !strings.Contains(string(data), "result text") {
		t.Fatalf("payload = %q", data)
	}
}
