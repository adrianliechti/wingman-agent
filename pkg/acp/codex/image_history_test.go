package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestUserInputImageReferences(t *testing.T) {
	for _, tc := range []struct {
		raw, text string
		metadata  map[string]any
	}{
		{`{"type":"image","url":"https://example.test/a.png"}`, "[@image](https://example.test/a.png)", nil},
		{`{"type":"image","fileId":"file-a","detail":"original"}`, "Image attachment (preview unavailable): file-a", map[string]any{"codex": map[string]any{"fileId": "file-a", "detail": "original"}}},
		{`{"type":"image","fileId":"file-b"}`, "Image attachment (preview unavailable): file-b", map[string]any{"codex": map[string]any{"fileId": "file-b"}}},
		{`{"type":"image"}`, "", nil},
		{`{"type":"image","fileId":42}`, "", nil},
	} {
		got, ok := userInputToBlock(json.RawMessage(tc.raw))
		if tc.text == "" {
			if ok {
				t.Errorf("invalid image produced a broken attachment: %+v", got)
			}
			continue
		}
		want := acp.TextBlock(tc.text)
		want.Text.Meta = tc.metadata
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got=%+v want=%+v", tc.raw, got, want)
		}
	}
}

func TestImageHistoryE2EThroughACP(t *testing.T) {
	const items = `[{"id":"user-images","type":"userMessage","content":[
		{"type":"text","text":"before"},
		{"type":"image","url":"https://example.test/a.png"},
		{"type":"image","fileId":"file-a","detail":"original"},
		{"type":"text","text":"between"},
		{"type":"image","fileId":"file-b"},
		{"type":"image","fileId":"file-a","detail":"original"}
	]}]`
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			app, backend := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = backend.Close() })
			go func() {
				scanner := bufio.NewScanner(backend)
				for scanner.Scan() {
					var request rpcMessage
					if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
						t.Error(err)
						return
					}
					if len(request.ID) == 0 {
						continue
					}
					response := rpcMessage{Jsonrpc: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)}
					switch request.Method {
					case "initialize", "thread/unsubscribe":
					case "model/list":
						response.Result = json.RawMessage(`{"data":[{"id":"image-model","displayName":"Image Model","isDefault":true}]}`)
					case "config/read":
						response.Result = json.RawMessage(`{"config":{}}`)
					case "thread/resume":
						response.Result = json.RawMessage(`{"thread":{"id":"images","turns":[]},"model":"image-model"}`)
					case "thread/read":
						var params threadReadParams
						_ = json.Unmarshal(request.Params, &params)
						if params.IncludeTurns {
							response.Result = json.RawMessage(fmt.Sprintf(`{"thread":{"id":"images","turns":[{"id":"t","items":%s}]}}`, items))
						} else {
							response.Result = json.RawMessage(fmt.Sprintf(`{"thread":{"id":"images","historyMode":%q}}`, mode))
						}
					case "thread/turns/list":
						response.Result = json.RawMessage(fmt.Sprintf(`{"data":[{"id":"t","items":%s}],"nextCursor":null}`, items))
					default:
						response.Result = nil
						response.Error = &rpcError{Code: -32601, Message: "method not found"}
					}
					writeRPCMessage(backend, response)
				}
			}()
			rpc := newRPCClient(app, app)
			rpc.start()
			peer := newProtocolPeer(t, newAgent(newCodexClient(rpc), "image-model", ""))
			peer.send(t, `"init"`, "initialize", `{"protocolVersion":1,"clientCapabilities":{}}`)
			if frame := peer.response(t, `"init"`); frame.Error != nil {
				t.Fatalf("initialize: %+v", frame.Error)
			}
			for load := range 2 {
				id := fmt.Sprintf(`"load-%d"`, load)
				peer.send(t, id, "session/load", `{"sessionId":"images","cwd":"/contract","mcpServers":[]}`)
				var blocks []acp.ContentBlock
				var metadata []map[string]any
				for {
					frame := peer.next(t)
					if string(frame.ID) == id {
						if frame.Error != nil {
							t.Fatalf("session/load: %+v", frame.Error)
						}
						break
					}
					var notification acp.SessionNotification
					if err := json.Unmarshal(frame.Params, &notification); err != nil {
						t.Fatal(err)
					}
					if chunk := notification.Update.UserMessageChunk; chunk != nil {
						if notification.SessionId != "images" || chunk.MessageId == nil || *chunk.MessageId != "user-images" {
							t.Fatalf("lost message identity: %+v", notification)
						}
						blocks = append(blocks, chunk.Content)
						metadata = append(metadata, chunk.Meta)
					}
				}
				want := []string{"before", "[@image](https://example.test/a.png)", "Image attachment (preview unavailable): file-a", "between", "Image attachment (preview unavailable): file-b", "Image attachment (preview unavailable): file-a"}
				if len(blocks) != len(want) {
					t.Fatalf("replayed %d blocks, want %d", len(blocks), len(want))
				}
				for i, text := range want {
					if blocks[i].Text == nil || blocks[i].Text.Text != text {
						t.Fatalf("block %d: %+v, want %s", i, blocks[i], text)
					}
				}
				for _, i := range []int{2, 4, 5} {
					wantMeta := map[string]any{"fileId": "file-a", "detail": "original"}
					if i == 4 {
						wantMeta = map[string]any{"fileId": "file-b"}
					}
					if !reflect.DeepEqual(metadata[i], map[string]any{"codex": wantMeta}) {
						t.Fatalf("block %d lost image metadata: %+v", i, metadata[i])
					}
				}
			}
		})
	}
}
