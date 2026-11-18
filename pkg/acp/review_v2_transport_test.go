package acp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

type requestIDAgent struct{ *versionedAgent }

func (*requestIDAgent) ListSessions(context.Context, acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	return acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}, nil
}

// JSON-RPC string IDs are compared by value, regardless of their escape
// spelling. Numeric IDs must keep their precision and remain distinct from strings.
func TestV2PromptCompletesWithRequestIDs(t *testing.T) {

	for _, id := range []any{"prompt", "<prompt>", "prompt&reply", "line\u2028separator", "1", json.Number("1"), json.Number("9007199254740993")} {
		t.Run(fmt.Sprintf("%T=%v", id, id), func(t *testing.T) {
			testV2PromptRequestID(t, id)
		})
	}
}

func testV2PromptRequestID(t *testing.T, id any) {
	t.Helper()
	agent := &requestIDAgent{versionedAgent: &versionedAgent{model: "one"}}
	agent.prompt = func(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "message")
		err := agent.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: p.SessionId, Update: acpsdk.UpdateAgentMessageText("answer")})
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, err
	}
	server, client := net.Pipe()
	connection := acp.NewAgentConnection(agent, server, server)
	t.Cleanup(func() { _ = client.Close(); _ = server.Close(); <-connection.Done() })
	frames := make(chan any, 16)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(client)
		for scanner.Scan() {
			var frame any
			decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
			decoder.UseNumber()
			if decoder.Decode(&frame) == nil {
				frames <- frame
			}
		}
	}()
	encoder := json.NewEncoder(client)
	encoder.SetEscapeHTML(false)
	request := func(requestID any, method string, params any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method, "params": params}
	}
	send := func(frame any) {
		t.Helper()
		_ = client.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := encoder.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	next := func() any {
		t.Helper()
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("connection closed before the turn finished")
			}
			return frame
		case <-time.After(2 * time.Second):
			t.Fatal("accepted prompt stopped streaming before reaching idle")
			return nil
		}
	}
	response := func(frame any, wantID any) map[string]any {
		t.Helper()
		object, ok := frame.(map[string]any)
		if !ok || object["id"] != wantID || object["error"] != nil {
			t.Fatalf("response for ID %v: %v", wantID, frame)
		}
		result, ok := object["result"].(map[string]any)
		if !ok {
			t.Fatalf("missing result: %v", frame)
		}
		return result
	}
	send(request("init", "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "test", "version": "1"}}))
	response(next(), "init")
	send(request("<new&session>", "session/new", map[string]any{"cwd": t.TempDir()}))
	response(next(), "<new&session>")
	prompt := request(id, "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "text", "text": "hello"}}})
	send(prompt)
	acknowledged, answer := false, false
	for {
		frame := next()
		object, ok := frame.(map[string]any)
		if !ok {
			t.Fatalf("invalid frame: %v", frame)
		}
		if object["id"] != nil {
			acknowledged = response(frame, id)["messageId"] == "message"
		}
		params, _ := object["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if update["sessionUpdate"] == "agent_message_chunk" {
			if !acknowledged {
				t.Fatal("answer arrived before prompt acknowledgement")
			}
			answer = true
		}
		if update["state"] == "idle" {
			if !acknowledged || !answer || update["stopReason"] != "end_turn" {
				t.Fatalf("incomplete turn: acknowledged=%v answer=%v idle=%v", acknowledged, answer, update)
			}
			return
		}
	}
}
