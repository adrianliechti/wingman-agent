package claude

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
)

// stubClient implements the acp.Client methods that the live test clients in
// this package don't care about, so each one only needs to define the
// methods its assertions actually exercise.
type stubClient struct{}

func (stubClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.ErrUnsupported
}
func (stubClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}
func (stubClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.ErrUnsupported
}
func (stubClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.ErrUnsupported
}
func (stubClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.ErrUnsupported
}
func (stubClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.ErrUnsupported
}
func (stubClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.ErrUnsupported
}

type testClient struct {
	stubClient
	mu        sync.Mutex
	text      strings.Builder
	permCalls int
}

func (c *testClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permCalls++
	c.mu.Unlock()
	if len(p.Options) == 0 {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: p.Options[0].OptionId},
	}}, nil
}

func (c *testClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	if u := n.Update.AgentMessageChunk; u != nil && u.Content.Text != nil {
		c.mu.Lock()
		c.text.WriteString(u.Content.Text.Text)
		c.mu.Unlock()
	}
	return nil
}

func TestLiveSession(t *testing.T) {
	if os.Getenv("CLAUDE_ACP_LIVE") == "" {
		t.Skip("set CLAUDE_ACP_LIVE=1 to run the live claude integration test")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not found: %v", err)
	}

	agentSide, clientSide := net.Pipe()
	agent := New(Options{Env: os.Environ(), Path: path})
	conn := acp.NewAgentSideConnection(agent, agentSide, agentSide)
	agent.SetAgentConnection(conn)

	client := &testClient{}
	cc := acp.NewClientSideConnection(client, clientSide, clientSide)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := cc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cwd, _ := os.MkdirTemp("", "claude-live")
	defer os.RemoveAll(cwd)
	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	r1, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Create a file named live.txt containing the word PINEAPPLE using the Write tool, then stop.")},
	})
	if err != nil {
		t.Fatalf("prompt 1: %v", err)
	}
	if r1.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("prompt 1 stop = %q, want end_turn", r1.StopReason)
	}
	client.mu.Lock()
	perms := client.permCalls
	client.mu.Unlock()
	if perms == 0 {
		t.Fatalf("expected at least one permission request for the Write tool")
	}
	if _, err := os.Stat(cwd + "/live.txt"); err != nil {
		t.Fatalf("live.txt not created (permission round-trip failed): %v", err)
	}

	client.mu.Lock()
	client.text.Reset()
	client.mu.Unlock()
	if _, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("What word did you just write into that file? Reply with only the word.")},
	}); err != nil {
		t.Fatalf("prompt 2: %v", err)
	}
	client.mu.Lock()
	got := client.text.String()
	client.mu.Unlock()
	if !strings.Contains(strings.ToUpper(got), "PINEAPPLE") {
		t.Fatalf("turn 2 lost context; reply = %q", got)
	}

	if _, err := cc.CloseSession(ctx, acp.CloseSessionRequest{SessionId: ns.SessionId}); err != nil {
		t.Fatalf("close session: %v", err)
	}
}
