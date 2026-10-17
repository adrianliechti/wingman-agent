package claude

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

// toolStateClient approves every permission and records each tool call's
// status, parent, and how many terminal updates it received.
type toolStateClient struct {
	stubClient
	mu        sync.Mutex
	name      map[acp.ToolCallId]string
	parent    map[acp.ToolCallId]string
	status    map[acp.ToolCallId]acp.ToolCallStatus
	terminals map[acp.ToolCallId]int
	text      strings.Builder
}

func newToolStateClient() *toolStateClient {
	return &toolStateClient{
		name: map[acp.ToolCallId]string{}, parent: map[acp.ToolCallId]string{},
		status: map[acp.ToolCallId]acp.ToolCallStatus{}, terminals: map[acp.ToolCallId]int{},
	}
}

func (c *toolStateClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	for _, option := range p.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
				Selected: &acp.RequestPermissionOutcomeSelected{OptionId: option.OptionId},
			}}, nil
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
}

func claudeMetaString(meta any, key string) string {
	m, _ := meta.(map[string]any)
	claude, _ := m["claudeCode"].(map[string]any)
	value, _ := claude[key].(string)
	return value
}

func (c *toolStateClient) record(id acp.ToolCallId, status *acp.ToolCallStatus, meta any) {
	if name := claudeMetaString(meta, "toolName"); name != "" {
		c.name[id] = name
	}
	if parent := claudeMetaString(meta, "parentToolUseId"); parent != "" {
		c.parent[id] = parent
	}
	if status == nil {
		return
	}
	c.status[id] = *status
	if *status == acp.ToolCallStatusCompleted || *status == acp.ToolCallStatusFailed {
		c.terminals[id]++
	}
}

func (c *toolStateClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := n.Update.AgentMessageChunk; u != nil && u.Content.Text != nil {
		c.text.WriteString(u.Content.Text.Text)
	}
	if u := n.Update.ToolCall; u != nil {
		status := u.Status
		c.record(u.ToolCallId, &status, u.Meta)
	}
	if u := n.Update.ToolCallUpdate; u != nil {
		c.record(u.ToolCallId, u.Status, u.Meta)
	}
	return nil
}

// runningBash reports a Bash call whose parent matches wantSubagent.
func (c *toolStateClient) runningBash(wantSubagent bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, name := range c.name {
		if name == "Bash" && (c.parent[id] != "") == wantSubagent && c.terminals[id] == 0 {
			return true
		}
	}
	return false
}

// TestLiveCancelEndsEveryToolCall cancels a running root Bash call and a
// running subagent Bash call through the installed Claude CLI. Every tool call
// must end, and none may be finalized twice. It makes short model requests,
// preferring a Haiku model when the CLI offers one.
func TestLiveCancelEndsEveryToolCall(t *testing.T) {
	if os.Getenv("CLAUDE_ACP_LIVE") == "" {
		t.Skip("set CLAUDE_ACP_LIVE=1 to run the live claude cancel test")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not found: %v", err)
	}
	for _, tt := range []struct {
		name     string
		prompt   string
		subagent bool
	}{
		{"root bash", "Use the Bash tool in the foreground to run exactly this command: for i in $(seq 1 90); do echo tick $i; sleep 1; done. Do not run anything else.", false},
		{"subagent bash", "Use the Agent tool with run_in_background explicitly set to false to start one general-purpose subagent in the foreground. Instruct it to use its Bash tool with run_in_background explicitly false to run exactly this command: for i in $(seq 1 90); do echo tick $i; sleep 1; done. Wait for the subagent to finish before replying. Do not run anything yourself.", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agentSide, clientSide := net.Pipe()
			t.Cleanup(func() { _ = agentSide.Close(); _ = clientSide.Close() })
			agent := New(Options{Env: os.Environ(), Path: path})
			conn := acp.NewAgentSideConnection(agent, agentSide, agentSide)
			agent.SetAgentConnection(conn)
			client := newToolStateClient()
			cc := acp.NewClientSideConnection(client, clientSide, clientSide)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if _, err := cc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			cwd := t.TempDir()
			ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			defer func() { _, _ = cc.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: ns.SessionId}) }()
			for _, option := range ns.ConfigOptions {
				if option.Select == nil || option.Select.Id != modelConfigID || option.Select.Options.Ungrouped == nil {
					continue
				}
				for _, value := range *option.Select.Options.Ungrouped {
					if strings.Contains(strings.ToLower(string(value.Value)), "haiku") {
						if _, err := cc.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
							SessionId: ns.SessionId, ConfigId: modelConfigID, Value: value.Value,
						}}); err != nil {
							t.Fatalf("select %s: %v", value.Value, err)
						}
						t.Logf("model %s", value.Value)
						break
					}
				}
			}

			type outcome struct {
				resp acp.PromptResponse
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				resp, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(tt.prompt)}})
				done <- outcome{resp, err}
			}()

			deadline := time.After(2 * time.Minute)
			for !client.runningBash(tt.subagent) {
				select {
				case r := <-done:
					client.mu.Lock()
					t.Logf("tools %v parents %v status %v", client.name, client.parent, client.status)
					t.Logf("text %q", client.text.String())
					client.mu.Unlock()
					t.Fatalf("prompt ended before the Bash call started: %+v %v", r.resp, r.err)
				case <-deadline:
					t.Fatal("Bash call did not start")
				case <-time.After(200 * time.Millisecond):
				}
			}
			time.Sleep(3 * time.Second) // let the command run
			if err := cc.Cancel(ctx, acp.CancelNotification{SessionId: ns.SessionId}); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			r := <-done
			if r.err != nil || r.resp.StopReason != acp.StopReasonCancelled {
				t.Fatalf("prompt = %+v, %v; want cancelled", r.resp, r.err)
			}

			// Notifications sent before the response can be handled after it.
			for wait := time.Now().Add(3 * time.Second); time.Now().Before(wait); time.Sleep(100 * time.Millisecond) {
				client.mu.Lock()
				open := false
				for id := range client.name {
					open = open || client.terminals[id] == 0
				}
				client.mu.Unlock()
				if !open {
					break
				}
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.name) == 0 {
				t.Fatal("no tool calls recorded")
			}
			for id, name := range client.name {
				status := client.status[id]
				t.Logf("%s %s parent=%q status=%s terminal updates=%d", id, name, client.parent[id], status, client.terminals[id])
				if client.terminals[id] != 1 {
					t.Errorf("tool call %s (%s) received %d terminal updates, want 1; status %s", id, name, client.terminals[id], status)
				}
			}
		})
	}
}
