package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

// These regressions use the real app-server through an ACP connection. Neither
// scenario sends a model prompt: settings and MCP startup are CLI operations.
func TestLiveCodexUnpromptedPlanMode(t *testing.T) {
	cc, ctx, cwd := startLiveCodex(t, &liveClient{}, 60*time.Second)
	for _, operation := range []string{"resume", "load"} {
		t.Run(operation, func(t *testing.T) {
			ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			configured, err := cc.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
				ValueId: &acp.SetSessionConfigOptionValueId{
					SessionId: ns.SessionId, ConfigId: collaborationModeConfigID, Value: planCollaborationMode,
				},
			})
			if err != nil {
				t.Fatalf("select Plan mode: %v", err)
			}
			if got := liveCollaborationMode(configured.ConfigOptions); got != planCollaborationMode {
				t.Fatalf("selected collaboration mode = %q, want plan", got)
			}

			var options []acp.SessionConfigOption
			if operation == "resume" {
				resp, err := cc.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: ns.SessionId, Cwd: cwd, McpServers: []acp.McpServer{}})
				if err != nil {
					t.Fatalf("resume unprompted session: %v", err)
				}
				options = resp.ConfigOptions
			} else {
				resp, err := cc.LoadSession(ctx, acp.LoadSessionRequest{SessionId: ns.SessionId, Cwd: cwd, McpServers: []acp.McpServer{}})
				if err != nil {
					t.Fatalf("load unprompted session: %v", err)
				}
				options = resp.ConfigOptions
			}
			if got := liveCollaborationMode(options); got != planCollaborationMode {
				t.Fatalf("%s changed the unprompted session's reported collaboration mode from plan to %q", operation, got)
			}
		})
	}
}

func liveCollaborationMode(options []acp.SessionConfigOption) string {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == collaborationModeConfigID {
			return string(option.Select.CurrentValue)
		}
	}
	return ""
}

func TestLiveCodexMCPStartupSessionIsolation(t *testing.T) {
	cc, ctx, cwd, agent := startLiveCodexAgent(t, &liveClient{}, 60*time.Second)
	const serverName = "acplive_startup_isolation"
	type startupEvent struct {
		ThreadID string `json:"threadId"`
		Name     string `json:"name"`
		Status   string `json:"status"`
	}
	events := make(chan startupEvent, 32)
	// Observe real notifications while retaining the adapter's normal dispatch.
	agent.codex.setGlobalNotificationHandler(func(threadID, method string, params json.RawMessage) {
		agent.handleGlobalNotification(threadID, method, params)
		if method == "mcpServer/startupStatus/updated" {
			var event startupEvent
			if json.Unmarshal(params, &event) == nil && event.Name == serverName {
				select {
				case events <- event:
				case <-ctx.Done():
				}
			}
		}
	})
	waitStatus := func(matches func(startupEvent) bool) startupEvent {
		t.Helper()
		for {
			select {
			case event := <-events:
				t.Logf("MCP startup: thread=%s status=%s", event.ThreadID, event.Status)
				if event.ThreadID == "" {
					t.Fatal("the CLI did not scope its MCP startup notification to a thread")
				}
				if event.Status == "failed" || event.Status == "cancelled" {
					t.Fatalf("MCP test server did not start: %+v", event)
				}
				if matches(event) {
					return event
				}
			case <-ctx.Done():
				t.Fatalf("waiting for MCP startup notification: %v", ctx.Err())
			}
		}
	}

	helper, _, _ := acptest.CommandHelper(t, "TestMCPStartupBarrierHelper", "ACP_LIVE_MCP_SERVER")
	server := func() ([]acp.McpServer, func()) {
		dir := t.TempDir()
		release := func() {
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
				t.Errorf("release MCP startup: %v", err)
			}
		}
		t.Cleanup(release)
		return []acp.McpServer{{Stdio: &acp.McpServerStdio{
			Name: serverName, Command: helper, Args: []string{},
			Env: []acp.EnvVariable{
				{Name: "ACP_LIVE_MCP_SERVER", Value: "1"},
				{Name: "ACP_LIVE_MCP_STARTUP_DIR", Value: dir},
			},
		}}}, release
	}
	firstServers, releaseFirst := server()
	first, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: firstServers})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	waitStatus(func(e startupEvent) bool { return e.ThreadID == string(first.SessionId) && e.Status == "starting" })

	secondServers, releaseSecond := server()
	type sessionResult struct {
		response acp.NewSessionResponse
		err      error
	}
	secondDone := make(chan sessionResult, 1)
	go func() {
		resp, err := cc.NewSession(ctx, acp.NewSessionRequest{
			Cwd: cwd, McpServers: secondServers,
			Meta: map[string]any{"mcpStartupAwaitTimeoutMs": 20000},
		})
		secondDone <- sessionResult{resp, err}
	}()
	second := waitStatus(func(e startupEvent) bool { return e.ThreadID != string(first.SessionId) && e.Status == "starting" })
	releaseFirst()
	waitStatus(func(e startupEvent) bool { return e.ThreadID == string(first.SessionId) && e.Status == "ready" })

	// The second helper cannot complete its handshake until we release it.
	// Returning now therefore cannot be justified by its own readiness.
	select {
	case result := <-secondDone:
		if result.err != nil {
			t.Fatalf("second session: %v", result.err)
		}
		t.Fatalf("session/new returned %s after %s became ready while its own MCP server remained blocked (thread %s)", result.response.SessionId, first.SessionId, second.ThreadID)
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseSecond()
	waitStatus(func(e startupEvent) bool { return e.ThreadID == second.ThreadID && e.Status == "ready" })
	select {
	case result := <-secondDone:
		if result.err != nil || string(result.response.SessionId) != second.ThreadID {
			t.Fatalf("second session after its own startup: id=%s err=%v", result.response.SessionId, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// This is a real stdio MCP server whose handshake can be held independently
// for each session. No Codex notifications or ACP responses are fabricated.
func TestMCPStartupBarrierHelper(t *testing.T) {
	dir := os.Getenv("ACP_LIVE_MCP_STARTUP_DIR")
	if os.Getenv("ACP_LIVE_MCP_SERVER") == "" || dir == "" {
		t.Skip("MCP startup helper process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			acptest.MCPServerHelper(t)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "MCP startup test barrier was not released")
			os.Exit(1)
		}
	}
}
