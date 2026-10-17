package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
	"github.com/coder/acp-go-sdk"
)

// Runs as an actual CLI subprocess, with stdin/stdout JSONL pipes.
func TestClaudeDeliveryHelper(t *testing.T) {
	if os.Getenv("CLAUDE_DELIVERY_HELPER") == "" {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var input cliInput
		if json.Unmarshal(scanner.Bytes(), &input) != nil || input.Type != "user" || len(input.Message.Content) == 0 {
			continue
		}
		switch input.Message.Content[0].Text {
		case "stream":
			_ = enc.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "LOST_OUTPUT"}}})
		case "assistant":
			_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "LOST_OUTPUT"}}}})
		case "rate limit":
			_ = enc.Encode(map[string]any{"type": "rate_limit_event", "status": "LOST_OUTPUT"})
		case "usage":
			_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": "claude-test", "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}, "content": []any{map[string]any{"type": "text", "text": "answer"}}}})
		case "tool result", "tool progress":
			_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "pwd"}}}}})
			if input.Message.Content[0].Text == "tool result" {
				_ = enc.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "LOST_OUTPUT"}}}})
			} else {
				_ = enc.Encode(map[string]any{"type": "tool_progress", "tool_use_id": "tool-1", "elapsed_time_seconds": 2})
			}
		default:
			_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "recovered"}}}})
		}
		_ = enc.Encode(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": input.UUID, "usage": map[string]any{"output_tokens": 1}, "modelUsage": map[string]any{"claude-test": map[string]any{"contextWindow": 200000}}})
	}
	os.Exit(0)
}

type failingDeliveryWriter struct {
	io.Writer
	needle []byte
	s      *session
	mu     sync.Mutex
	failed *claudeProc
}

func (w *failingDeliveryWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, w.needle) {
		w.s.mu.Lock()
		proc := w.s.proc
		w.s.mu.Unlock()
		w.mu.Lock()
		w.failed = proc
		w.mu.Unlock()
		return 0, errors.New("injected ACP write failure")
	}
	return w.Writer.Write(b)
}

func TestClaudeDeliveryFailureE2E(t *testing.T) {
	for _, kind := range []string{"stream", "assistant", "tool result", "tool progress", "rate limit", "usage"} {
		t.Run(kind, func(t *testing.T) {
			script, dir, env := acptest.CommandHelper(t, "TestClaudeDeliveryHelper", "CLAUDE_DELIVERY_HELPER")
			env = append(env, "GORACE=atexit_sleep_ms=0")
			a := New(Options{Path: script, Env: env, Stderr: io.Discard})
			t.Cleanup(func() { _ = a.Close() })
			// Model discovery is unrelated to delivery and would start a second CLI.
			a.modelsLoaded = true
			serverIO, clientIO := net.Pipe()
			t.Cleanup(func() { _ = serverIO.Close(); _ = clientIO.Close() })
			writer := &failingDeliveryWriter{Writer: serverIO, needle: []byte("LOST_OUTPUT")}
			if kind == "tool progress" {
				writer.needle = []byte("elapsedTimeSeconds")
			} else if kind == "usage" {
				writer.needle = []byte("usage_update")
			}
			a.SetAgentConnection(acp.NewAgentSideConnection(a, writer, serverIO))
			_ = acp.NewClientSideConnection(&testClient{}, clientIO, clientIO)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			ns, err := a.NewSession(ctx, acp.NewSessionRequest{Cwd: dir, McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatal(err)
			}
			s := a.lookup(ns.SessionId)
			writer.s = s
			// Call the public handler to observe its result independently of the
			// broken transport. The SDK still writes real ACP notifications over
			// net.Pipe; an RPC caller alone would see EOF even with the old bug.
			resp, err := a.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(kind)}})
			if err == nil || !strings.Contains(err.Error(), "injected ACP write failure") || resp.StopReason == acp.StopReasonEndTurn {
				t.Fatalf("failed delivery reported success: %+v, %v", resp, err)
			}
			writer.mu.Lock()
			failed := writer.failed
			writer.mu.Unlock()
			if failed == nil || failed.cmd.ProcessState == nil || !failed.cmd.ProcessState.Exited() {
				t.Fatal("failed CLI process was not reaped")
			}
			s.mu.Lock()
			retired := s.proc == nil
			s.mu.Unlock()
			if !retired {
				t.Fatal("session retained the failed process")
			}

			// Reconnect the client and send another prompt through both SDK ends.
			// The session must spawn a new process and deliver the answer once.
			nextServerIO, nextClientIO := net.Pipe()
			t.Cleanup(func() { _ = nextServerIO.Close(); _ = nextClientIO.Close() })
			a.SetAgentConnection(acp.NewAgentSideConnection(a, nextServerIO, nextServerIO))
			client := &testClient{}
			conn := acp.NewClientSideConnection(client, nextClientIO, nextClientIO)
			resp, err = conn.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("recover")}})
			client.mu.Lock()
			text := client.text.String()
			client.mu.Unlock()
			if err != nil || resp.StopReason != acp.StopReasonEndTurn || text != "recovered" {
				t.Fatalf("next prompt: %+v error=%v text=%q", resp, err, text)
			}
		})
	}
}
