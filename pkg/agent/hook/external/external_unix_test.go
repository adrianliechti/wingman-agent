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

	hooks := cfg.PreHooks(t.TempDir())
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

	result, err := cfg.PreHooks(t.TempDir())[0](context.Background(), tool.ToolCall{Name: "shell"})
	if err != nil || result != "" {
		t.Fatalf("passing hook blocked: %q, %v", result, err)
	}
}

func TestPostHookAppendsOutput(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "echo reviewed"}}}

	result, err := cfg.PostHooks(t.TempDir())[0](context.Background(), tool.ToolCall{Name: "shell"}, "original")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result, "original") || !strings.Contains(result, "<hook-output>\nreviewed\n</hook-output>") {
		t.Fatalf("result = %q", result)
	}
}

func TestPostHookKeepsResultOnFailure(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "echo broken; exit 1"}}}

	result, err := cfg.PostHooks(t.TempDir())[0](context.Background(), tool.ToolCall{Name: "shell"}, "original")
	if err != nil || result != "original" {
		t.Fatalf("result = %q, %v", result, err)
	}
}

func TestPostHookReceivesPayload(t *testing.T) {
	cfg := &Config{PostToolUse: []Rule{{Command: "cat"}}}

	result, err := cfg.PostHooks(t.TempDir())[0](context.Background(), tool.ToolCall{Name: "shell", Args: `{"command":"ls"}`}, "out")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"tool_name":"shell"`) || !strings.Contains(result, `"result":"out"`) {
		t.Fatalf("result = %q", result)
	}
}
