package claude

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

const livePermissionModeProbe = "ACP_LIVE_PERMISSION_MODE_PROBE: Reply with exactly READY. Do not use tools."

// Check the real CLI's mode on the next prompt, not just the adapter's
// current_mode_update. UserPromptSubmit supplies the mode in its hook input.
func TestLiveExitPlanModeEnablesBypass(t *testing.T) {
	var selected atomic.Int32
	client := &liveClient{pick: func(p acp.RequestPermissionRequest) acp.PermissionOptionId {
		for _, option := range p.Options {
			if option.OptionId == optionExitPlanBypass {
				selected.Add(1)
				return option.OptionId
			}
		}
		return p.Options[0].OptionId
	}}
	cc, ctx, cwd := startLiveConn(t, client)
	helper, _, _ := acptest.CommandHelper(t, "TestClaudePermissionModeHookHelper", "ACP_LIVE_CLAUDE_MODE_HOOK")
	recordedMode := filepath.Join(t.TempDir(), "permission-mode.json")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	settings := map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "env ACP_LIVE_CLAUDE_MODE_HOOK=1 " + quote(helper) + " " + quote(recordedMode),
		}}}},
	}}
	settingsDir := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := cc.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: ns.SessionId, ModeId: planModeID}); err != nil {
		t.Fatalf("select Plan mode: %v", err)
	}
	resp, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(
		"This is a permission-mode protocol test. Make a one-sentence plan for creating hello.txt, then call ExitPlanMode exactly once. Write a plan file if the tool requires it. After the user accepts, reply PLAN_ACCEPTED and stop. Do not implement the plan or inspect the workspace.",
	)}})
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("planning prompt: stop=%s err=%v", resp.StopReason, err)
	}
	_, _, modes := client.snapshot()
	if selected.Load() == 0 || !slices.Contains(modes, bypassModeID) {
		t.Fatalf("bypass was not selected through ACP: selections=%d modes=%v", selected.Load(), modes)
	}
	resp, err = cc.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(livePermissionModeProbe)}})
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("permission-mode probe: stop=%s err=%v", resp.StopReason, err)
	}
	data, err = os.ReadFile(recordedMode)
	if err != nil {
		t.Fatalf("the real CLI did not run the permission-mode probe hook: %v", err)
	}
	var actual string
	if err := json.Unmarshal(data, &actual); err != nil {
		t.Fatalf("decode hook's permission mode: %v", err)
	}
	if actual != "bypassPermissions" {
		t.Fatalf("ACP reported %q after the bypass choice, but the real Claude CLI's next prompt ran in %q", bypassModeID, actual)
	}
}

func TestClaudePermissionModeHookHelper(t *testing.T) {
	if os.Getenv("ACP_LIVE_CLAUDE_MODE_HOOK") == "" {
		t.Skip("Claude permission-mode hook process")
	}
	var input struct {
		Prompt         string `json:"prompt"`
		PermissionMode string `json:"permission_mode"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if input.Prompt == livePermissionModeProbe {
		args := flag.Args()
		if len(args) != 1 || input.PermissionMode == "" {
			fmt.Fprintln(os.Stderr, "permission-mode hook requires an output path and permission_mode")
			os.Exit(1)
		}
		data, _ := json.Marshal(input.PermissionMode)
		if err := os.WriteFile(args[0], data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}
