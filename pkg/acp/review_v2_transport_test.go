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

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	acpsdk "github.com/coder/acp-go-sdk"
)

type requestIDAgent struct{ *versionedAgent }

func (*requestIDAgent) ListSessions(context.Context, acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	return acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}, nil
}

// JSON-RPC string IDs are compared by value, regardless of their escape
// spelling. Numeric IDs must keep their precision and remain distinct from strings.
func TestV2PromptCompletesWithRequestIDs(t *testing.T) {
	for _, batched := range []bool{false, true} {
		for _, id := range []any{"prompt", "<prompt>", "prompt&reply", "line\u2028separator", "1", json.Number("1"), json.Number("9007199254740993")} {
			t.Run(fmt.Sprintf("batch=%t/%T=%v", batched, id, id), func(t *testing.T) {
				testV2PromptRequestID(t, id, batched)
			})
		}
	}
}

func testV2PromptRequestID(t *testing.T, id any, batched bool) {
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
	send(request("init", "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "test"}}))
	response(next(), "init")
	send(request("<new&session>", "session/new", map[string]any{"cwd": t.TempDir()}))
	response(next(), "<new&session>")
	prompt := request(id, "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "text", "text": "hello"}}})
	var listID any = "<list&sessions>"
	if number, ok := id.(json.Number); ok {
		listID = string(number)
	} else if id == "1" {
		listID = json.Number("1")
	}
	if batched {
		send([]any{prompt, request(listID, "session/list", map[string]any{})})
	} else {
		send(prompt)
	}
	acknowledged, answer := false, false
	for {
		frame := next()
		if batch, ok := frame.([]any); ok {
			if !batched || len(batch) != 2 || acknowledged {
				t.Fatalf("unexpected response batch: %v", batch)
			}
			for _, item := range batch {
				object, ok := item.(map[string]any)
				if !ok {
					t.Fatalf("invalid batch response: %v", item)
				}
				if object["id"] == id {
					acknowledged = response(item, id)["messageId"] == "message"
				} else {
					response(item, listID)
				}
			}
			if !acknowledged {
				t.Fatalf("missing prompt acknowledgement: %v", batch)
			}
			continue
		}
		object, ok := frame.(map[string]any)
		if !ok {
			t.Fatalf("invalid frame: %v", frame)
		}
		if object["id"] != nil {
			if batched {
				t.Fatalf("batch response sent outside its array: %v", frame)
			}
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
