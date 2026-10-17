package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/agent"
	acpsdk "github.com/coder/acp-go-sdk"
)

type streamingClient struct {
	recordingClient
	textMu sync.Mutex
	text   strings.Builder
}

func (c *streamingClient) SessionUpdate(ctx context.Context, notification acpsdk.SessionNotification) error {
	if err := c.recordingClient.SessionUpdate(ctx, notification); err != nil {
		return err
	}
	if chunk := notification.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
		c.textMu.Lock()
		c.text.WriteString(chunk.Content.Text.Text)
		c.textMu.Unlock()
	}
	return nil
}

func (c *streamingClient) receivedText() string {
	c.textMu.Lock()
	defer c.textMu.Unlock()
	return c.text.String()
}

// Crosses both ACP SDK connections, the real Wingman session/harness, and
// Responses HTTP/SSE. Only the model service is replaced by a local server.
func streamingE2EConnection(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*acpsdk.ClientSideConnection, acpsdk.SessionId, *streamingClient) {
	t.Helper()
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	unsetEnv(t, "WINGMAN_URL")
	unsetEnv(t, "WINGMAN_TOKEN")
	unsetEnv(t, "WINGMAN_MODEL")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_DEFAULT_MODEL", "stream-test")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stream-test","object":"model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		respond(w, r)
	}))
	t.Cleanup(backend.Close)
	t.Setenv("OPENAI_BASE_URL", backend.URL)
	cfg, err := agent.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireFinish = func(string) bool { return false }
	s := &contractServer{Server: &Server{
		config: cfg, sessions: map[acpsdk.SessionId]*sessionEntry{},
		sessionDirs: map[acpsdk.SessionId]string{}, workspaces: map[string]*workspaceEntry{},
	}, model: backend}
	t.Cleanup(func() { _ = s.Close() })
	serverIO, clientIO := net.Pipe()
	t.Cleanup(func() { _ = serverIO.Close(); _ = clientIO.Close() })
	s.SetAgentConnection(acpsdk.NewAgentSideConnection(s, serverIO, serverIO))
	client := &streamingClient{}
	conn := acpsdk.NewClientSideConnection(client, clientIO, clientIO)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := conn.Initialize(ctx, acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	session, err := conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	return conn, session.SessionId, client
}

func streamingTextItem(parts ...string) map[string]any {
	content := make([]map[string]any, 0, len(parts))
	for _, text := range parts {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{"type": "message", "id": "msg", "role": "assistant", "status": "completed", "content": content}
}

func TestACPFinalTextDeliveryE2E(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parts    []string
		deltas   []string
		itemDone bool
	}{
		{"final only", []string{"Hello, 世界!"}, nil, false},
		{"partial deltas", []string{"Hello, 世界!"}, []string{"Hello, 世"}, false},
		{"complete deltas", []string{"Hello, 世界!"}, []string{"Hello, 世界!"}, false},
		{"multiple parts", []string{"Hello, ", "世界!"}, []string{"Hello, ", "世"}, false},
		{"item done only", []string{"Hello, 世界!"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			conn, id, client := streamingE2EConnection(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				for i, delta := range tc.deltas {
					writeContractSSE(w, map[string]any{"type": "response.output_text.delta", "item_id": "msg", "content_index": i, "delta": delta})
				}
				item := streamingTextItem(tc.parts...)
				output := []map[string]any{item}
				if tc.itemDone {
					writeContractSSE(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
					output = nil
				}
				writeContractSSE(w, completedEvent(5, 1, 1, 0, 0, 0, output))
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := conn.Prompt(ctx, acpsdk.PromptRequest{SessionId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("answer")}})
			if err != nil || result.StopReason != acpsdk.StopReasonEndTurn || client.receivedText() != strings.Join(tc.parts, "") || requests.Load() != 1 {
				t.Fatalf("result=%+v error=%v received=%q requests=%d", result, err, client.receivedText(), requests.Load())
			}
		})
	}
}

func TestACPInvisibleMetadataRetryE2E(t *testing.T) {
	var requests atomic.Int64
	var inputsMu sync.Mutex
	var inputs []json.RawMessage
	conn, id, client := streamingE2EConnection(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		inputsMu.Lock()
		inputs = append(inputs, req.Input)
		inputsMu.Unlock()
		if requests.Add(1) == 1 {
			writeContractSSE(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "reason", "summary": []any{}}})
			writeContractSSE(w, map[string]any{"type": "response.output_item.added", "output_index": 1, "item": streamingTextItem("")})
			writeContractSSE(w, map[string]any{"type": "response.output_text.delta", "item_id": "msg", "content_index": 0, "delta": ""})
			writeContractSSE(w, map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "rate_limit_exceeded", "message": "busy"}}})
			return
		}
		writeContractSSE(w, completedEvent(2, 1, 1, 0, 0, 0, []map[string]any{streamingTextItem("recovered")}))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	result, err := conn.Prompt(ctx, acpsdk.PromptRequest{SessionId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("recover")}})
	if err != nil || result.StopReason != acpsdk.StopReasonEndTurn || client.receivedText() != "recovered" || requests.Load() != 2 {
		t.Fatalf("result=%+v error=%v received=%q requests=%d", result, err, client.receivedText(), requests.Load())
	}
	inputsMu.Lock()
	defer inputsMu.Unlock()
	if len(inputs) != 2 || string(inputs[0]) != string(inputs[1]) {
		t.Fatalf("retry changed input: %s", inputs)
	}
}

func TestACPDeliveredOutputStillPreventsRetryE2E(t *testing.T) {
	for _, kind := range []string{"text", "reasoning", "tool"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int64
			conn, id, client := streamingE2EConnection(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				switch kind {
				case "text":
					writeContractSSE(w, map[string]any{"type": "response.output_text.delta", "item_id": "msg", "delta": "partial"})
				case "reasoning":
					writeContractSSE(w, map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "reason", "summary_index": 0, "delta": "partial thought"})
				case "tool":
					writeContractSSE(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "glob", "arguments": "{}"}})
				}
				writeContractSSE(w, map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "server_error", "message": "failed"}}})
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := conn.Prompt(ctx, acpsdk.PromptRequest{SessionId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("work")}})
			if err == nil || requests.Load() != 1 || kind == "text" && client.receivedText() != "partial" {
				t.Fatalf("error=%v received=%q requests=%d", err, client.receivedText(), requests.Load())
			}
		})
	}
}
