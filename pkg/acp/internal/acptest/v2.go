package acptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

// RunV2 tests the public wire contract with the same backend fixtures as v1.
func RunV2(t *testing.T, factory Factory) {
	for _, cancelTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelTurn), func(t *testing.T) {
			agent := factory(t)
			server, client := net.Pipe()
			connected, ok := agent.(interface {
				acpsdk.Agent
				SetClientConnection(acp.ClientConnection)
			})
			if !ok {
				t.Fatalf("%T cannot negotiate ACP", agent)
			}
			acp.NewAgentConnection(connected, server, server)
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
				if closer, ok := agent.(closer); ok {
					_ = closer.Close()
				}
			})
			frames := make(chan map[string]any, 128)
			go func() {
				defer close(frames)
				scanner := bufio.NewScanner(client)
				scanner.Buffer(nil, 4*1024*1024)
				for scanner.Scan() {
					var frame map[string]any
					if json.Unmarshal(scanner.Bytes(), &frame) == nil {
						frames <- frame
					}
				}
			}()
			send := func(id, method string, params any) {
				t.Helper()
				frame := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
				if id != "" {
					frame["id"] = id
				}
				_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := json.NewEncoder(client).Encode(frame); err != nil {
					t.Fatal(err)
				}
			}
			next := func() map[string]any {
				t.Helper()
				select {
				case f, ok := <-frames:
					if !ok {
						t.Fatal("connection closed")
					}

					if f["error"] == nil {
						method, _ := f["method"].(string)
						names := map[string]string{"session/update": "UpdateSessionNotification", "session/request_permission": "RequestPermissionRequest", "elicitation/create": "CreateElicitationRequest"}
						if name := names[method]; name != "" {
							ValidateV2(t, name, f["params"])
						}
						if method == "" {
							names := map[string]string{"init": "InitializeResponse", "new": "NewSessionResponse", "prompt": "PromptResponse"}
							id, _ := f["id"].(string)
							if name := names[id]; name != "" {
								ValidateV2(t, name, f["result"])
							}
						}
					}
					return f
				case <-time.After(8 * time.Second):
					t.Fatal("timed out waiting for ACP frame")
				}
				return nil
			}
			response := func(id string) map[string]any {
				t.Helper()
				for {
					f := next()
					if f["id"] == id {
						if f["error"] != nil {
							t.Fatalf("%s: %v", id, f["error"])
						}
						r, _ := f["result"].(map[string]any)
						return r
					}
				}
			}
			send("init", "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "contract", "version": "1"}, "capabilities": map[string]any{}})
			init := response("init")
			if init["protocolVersion"] != float64(2) || init["info"] == nil || init["agentInfo"] != nil {
				t.Fatalf("initialize = %v", init)
			}
			send("new", "session/new", map[string]any{"cwd": t.TempDir()})
			created := response("new")
			id, _ := created["sessionId"].(string)
			if id == "" || created["modes"] != nil {
				t.Fatalf("session/new = %v", created)
			}
			options, _ := created["configOptions"].([]any)
			for _, v := range options {
				o := v.(map[string]any)
				if o["configId"] == nil || o["id"] != nil {
					t.Fatalf("v1 config leaked: %v", o)
				}
			}
			prompt := NormalPrompt
			if cancelTurn {
				prompt = CancelPrompt
			}
			send("prompt", "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": prompt}}})
			var acknowledged, user, running, tools, text, cancelled bool
			for {
				f := next()
				if f["id"] == "prompt" {
					if f["error"] != nil {
						t.Fatalf("prompt: %v", f["error"])
					}
					r := f["result"].(map[string]any)
					if r["messageId"] == nil || r["stopReason"] != nil || !user {
						t.Fatalf("prompt acknowledgement: %v, user=%v", r, user)
					}
					acknowledged = true
				}
				if f["method"] == "session/request_permission" {
					sendReply := map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}}}
					if err := json.NewEncoder(client).Encode(sendReply); err != nil {
						t.Fatal(err)
					}
				}
				if f["method"] != "session/update" {
					continue
				}
				p := f["params"].(map[string]any)
				u := p["update"].(map[string]any)
				switch u["sessionUpdate"] {
				case "user_message":
					user = true
				case "agent_message_chunk", "agent_thought_chunk":
					if u["messageId"] == nil || !user {
						t.Fatalf("message before acceptance or without id: %v", u)
					}
					b, _ := u["content"].(map[string]any)
					value, _ := b["text"].(string)
					text = text || strings.Contains(value, NormalText) || strings.Contains(value, CancelText)
				case "tool_call_update":
					tools = true
					if u["toolCallId"] == nil {
						t.Fatalf("tool without id: %v", u)
					}
				case "tool_call", "current_mode_update", "plan":
					t.Fatalf("v1 entity leaked: %v", u)
				case "state_update":
					if u["state"] == "running" {
						running = true
					}
					if u["state"] == "idle" {
						want := "end_turn"
						if cancelTurn {
							want = "cancelled"
						}
						if u["stopReason"] != want || !user || !running || !text || (!cancelTurn && !tools) {
							t.Fatalf("idle = %v; user=%v running=%v text=%v tools=%v", u, user, running, text, tools)
						}
						if !acknowledged {
							_ = response("prompt")
						}
						return
					}
				}
				if cancelTurn && acknowledged && text && !cancelled {
					send("", "session/cancel", map[string]any{"sessionId": id})
					cancelled = true
				}
			}
		})
	}
}
