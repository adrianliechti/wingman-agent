package codex

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

type liveClient struct {
	mu         sync.Mutex
	text       strings.Builder
	toolTitles []string
}

func (c *liveClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.ErrUnsupported
}
func (c *liveClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}
func (c *liveClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.ErrUnsupported
}
func (c *liveClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.ErrUnsupported
}
func (c *liveClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.ErrUnsupported
}
func (c *liveClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.ErrUnsupported
}
func (c *liveClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.ErrUnsupported
}

func (c *liveClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if len(p.Options) == 0 {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: p.Options[0].OptionId},
	}}, nil
}

func (c *liveClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := n.Update.AgentMessageChunk; u != nil && u.Content.Text != nil {
		c.text.WriteString(u.Content.Text.Text)
	}
	if u := n.Update.ToolCall; u != nil {
		c.toolTitles = append(c.toolTitles, u.Title)
	}
	return nil
}

func (c *liveClient) snapshot() (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String(), append([]string(nil), c.toolTitles...)
}

func startLiveCodex(t *testing.T, client acp.Client, timeout time.Duration) (*acp.ClientSideConnection, context.Context, string) {
	t.Helper()
	cc, ctx, cwd, _ := startLiveCodexAgent(t, client, timeout)
	return cc, ctx, cwd
}

func startLiveCodexAgent(t *testing.T, client acp.Client, timeout time.Duration) (*acp.ClientSideConnection, context.Context, string, *Agent) {
	t.Helper()
	if os.Getenv("CODEX_ACP_LIVE") == "" {
		t.Skip("set CODEX_ACP_LIVE=1 to run the live codex integration test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex not found: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	cwd, err := os.MkdirTemp("", "codex-live")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(cwd) })

	agent, err := Spawn(ctx, Options{Dir: cwd, Env: os.Environ()})
	if err != nil {
		t.Fatalf("spawn codex: %v", err)
	}
	t.Cleanup(func() { agent.Close() })

	agentSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close(); _ = clientSide.Close() })
	conn := acp.NewAgentSideConnection(agent, agentSide, agentSide)
	agent.SetAgentConnection(conn)
	cc := acp.NewClientSideConnection(client, clientSide, clientSide)

	if _, err := cc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return cc, ctx, cwd, agent
}

func TestLiveCodexSession(t *testing.T) {
	client := &liveClient{}
	cc, ctx, cwd := startLiveCodex(t, client, 120*time.Second)

	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	r, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly: OK")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if r.StopReason != acp.StopReasonEndTurn {
		t.Errorf("stop = %q, want end_turn", r.StopReason)
	}
	text, _ := client.snapshot()
	if !strings.Contains(strings.ToUpper(text), "OK") {
		t.Errorf("reply = %q, want OK", text)
	}
}

func TestLiveCodexSubagentActivity(t *testing.T) {
	client := &liveClient{}
	cc, ctx, cwd := startLiveCodex(t, client, 300*time.Second)

	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"Use your multi-agent collaboration tools: spawn exactly one subagent with the prompt \"Reply with the word hi\". Wait for it to finish, close it, then reply with exactly: DONE")},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	text, titles := client.snapshot()
	t.Logf("tool call titles: %q", titles)
	if !strings.Contains(strings.ToUpper(text), "DONE") {
		t.Errorf("reply = %q, want DONE", text)
	}
	sawCollab, sawSubagent := false, false
	for _, title := range titles {
		if strings.Contains(title, "agent") && !strings.Contains(title, "subagent") {
			sawCollab = true
		}
		if strings.Contains(title, "subagent") {
			sawSubagent = true
		}
	}
	if !sawCollab && !sawSubagent {
		t.Errorf("no collab or subagent tool calls surfaced; titles = %q", titles)
	}
}

func TestLiveCodexUnpromptedSessionLifecycle(t *testing.T) {
	cc, ctx, cwd := startLiveCodex(t, &liveClient{}, 60*time.Second)

	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	// Codex has not written a rollout for a thread without a user message.
	if _, err := cc.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: ns.SessionId, Cwd: cwd, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("resume unprompted session: %v", err)
	}
	if _, err := cc.LoadSession(ctx, acp.LoadSessionRequest{SessionId: ns.SessionId, Cwd: cwd, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("load unprompted session: %v", err)
	}
	for range 2 {
		if _, err := cc.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: ns.SessionId}); err != nil {
			t.Fatalf("delete unprompted session: %v", err)
		}
	}
	if _, err := cc.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "not-a-codex-thread"}); err != nil {
		t.Fatalf("delete unknown session: %v", err)
	}
}

func TestLiveCodexMCPStartupAwait(t *testing.T) {
	cc, ctx, cwd := startLiveCodex(t, &liveClient{}, 60*time.Second)
	const helperEnv = "ACP_LIVE_MCP_SERVER"
	helper, _, _ := acptest.CommandHelper(t, "TestMCPServerHelper", helperEnv)
	start := time.Now()
	_, err := cc.NewSession(ctx, acp.NewSessionRequest{
		Cwd: cwd,
		McpServers: []acp.McpServer{{Stdio: &acp.McpServerStdio{
			Name: "acplive", Command: helper, Args: []string{},
			Env: []acp.EnvVariable{{Name: helperEnv, Value: "1"}},
		}}},
		Meta: map[string]any{"mcpStartupAwaitTimeoutMs": 30000},
	})
	if err != nil {
		t.Fatalf("new session awaiting MCP startup: %v", err)
	}
	// Reaching the timeout would mean the startup status was never matched.
	if waited := time.Since(start); waited > 25*time.Second {
		t.Fatalf("session/new waited %v for a fast MCP server", waited)
	}
}
