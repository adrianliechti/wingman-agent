package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func dialEditorTest(t *testing.T, app *Server, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	web := httptest.NewServer(app.checkInstance(app.mux))
	t.Cleanup(web.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, strings.Replace(web.URL, "http:", "ws:", 1)+path+"?instance="+app.scope.InstanceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, ctx
}
func editorTestReply(t *testing.T, conn *websocket.Conn, ctx context.Context, request map[string]any) map[string]any {
	t.Helper()
	if err := wsjson.Write(ctx, conn, request); err != nil {
		t.Fatal(err)
	}
	for {
		var result map[string]any
		if err := wsjson.Read(ctx, conn, &result); err != nil {
			t.Fatal(err)
		}
		if result["id"] == request["id"] && request["id"] != nil || result["request_seq"] == request["seq"] && request["seq"] != nil {
			return result
		}
	}
}

func TestLSPWebSocketDocumentVersionsAndScope(t *testing.T) {
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc Disk() {}\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	conn, ctx := dialEditorTest(t, app, "/lsp")
	call := func(id float64, method string, params any) map[string]any {
		return editorTestReply(t, conn, ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	if response := call(1, "textDocument/hover", map[string]any{}); response["error"].(map[string]any)["code"] != float64(-32002) {
		t.Fatalf("before initialization: %v", response)
	}
	response := call(2, "initialize", map[string]any{"processId": nil, "capabilities": map[string]any{}})
	if response["result"].(map[string]any)["capabilities"].(map[string]any)["positionEncoding"] != "utf-16" {
		t.Fatal(response)
	}
	uri := fileuri.FromPath(filepath.Join(root, "main.go"))
	notify := func(method string, params any) {
		if err := wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
	}
	notify("initialized", map[string]any{})
	notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": "package main\nfunc Unsaved() {}\n"}})
	notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2}, "contentChanges": []any{map[string]any{"text": "package main\nfunc Current() {}\n"}}})
	// An older version cannot overwrite the current buffer.
	notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 1}, "contentChanges": []any{map[string]any{"text": "package main\nfunc Stale() {}\n"}}})
	response = call(3, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}})
	wire, _ := json.Marshal(response)
	if !strings.Contains(string(wire), "Current") || strings.Contains(string(wire), "Stale") || strings.Contains(string(wire), "Disk") {
		t.Fatalf("unsaved symbols: %s", wire)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "main.go")); !strings.Contains(string(data), "Disk") {
		t.Fatal("language synchronization wrote the file")
	}
	response = call(4, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": fileuri.FromPath(filepath.Join(root, "..", "outside.go"))}, "position": map[string]any{"line": 0, "character": 0}})
	if response["error"] == nil {
		t.Fatal("outside document accepted")
	}
	response = call(5, "textDocument/semanticTokens/full", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if response["error"] != nil {
		t.Fatal(response)
	}
	tokens := response["result"].(map[string]any)["data"].([]any)
	if len(tokens) == 0 || len(tokens)%5 != 0 {
		t.Fatal(tokens)
	}
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if response := call(6, "shutdown", nil); response["error"] != nil {
		t.Fatal(response)
	}
	app.lspDocumentsMu.Lock()
	remaining := len(app.lspDocuments)
	app.lspDocumentsMu.Unlock()
	if remaining != 0 {
		t.Fatal("closing a deleted file retained its language document")
	}
	if response := call(7, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}}); response["error"] == nil {
		t.Fatal("request after shutdown accepted")
	}
}

func TestLegacyProtocolRoutesAreRemoved(t *testing.T) {
	app := newDebugTestServer(t, t.TempDir())
	for _, route := range []string{
		"GET /api/v2/events",
		"GET /api/v2/backends/one/sessions",
		"POST /api/v2/backends/one/sessions",
		"POST /api/v2/backends/one/sessions/saved/commands",
		"GET /api/terminals", "POST /api/terminals",
		"POST /api/lsp/hover", "POST /api/lsp/document",
		"GET /api/debug/state", "POST /api/debug/start", "POST /api/debug/control",
	} {
		t.Run(route, func(t *testing.T) {
			method, path, _ := strings.Cut(route, " ")
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("legacy route returned %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDAPWebSocketBreakpointsAndReferenceLifetime(t *testing.T) {
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc main() {}\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	conn, ctx := dialEditorTest(t, app, "/dap")
	call := func(seq float64, command string, args any) map[string]any {
		return editorTestReply(t, conn, ctx, map[string]any{"seq": seq, "type": "request", "command": command, "arguments": args})
	}
	if response := call(1, "threads", nil); response["success"] != false {
		t.Fatal(response)
	}
	response := call(2, "initialize", map[string]any{"adapterID": "wingman", "linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path"})
	if response["success"] != true || response["body"].(map[string]any)["supportsConfigurationDoneRequest"] != true {
		t.Fatal(response)
	}
	response = call(3, "setBreakpoints", map[string]any{"source": map[string]any{"path": filepath.Join(root, "main.go")}, "breakpoints": []any{map[string]any{"line": 2, "condition": "x > 0", "hitCondition": "3"}}})
	if response["success"] != true {
		t.Fatal(response)
	}
	response = call(4, "wingman/status", map[string]any{"source": map[string]any{"path": "main.go"}})
	points := response["body"].(map[string]any)["breakpoints"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["hit_condition"] != "3" {
		t.Fatal(response)
	}
	if response := call(5, "setBreakpoints", map[string]any{"source": map[string]any{"path": "../main.go"}}); response["success"] != false {
		t.Fatal(response)
	}
	if response := call(6, "launch", map[string]any{"program": "/tmp/arbitrary"}); response["success"] != false {
		t.Fatal("unreviewed launch accepted", response)
	}
	// Adapter reference numbers may be reused at the next stop. Browser IDs
	// must not be reusable across either stops or WebSocket connections.
	c := &dapConnection{server: app, references: map[int]dapReference{}}
	status := dap.Status{SessionID: "debug", State: dap.StateStopped, StateVersion: 1}
	first, _ := c.reference(status, 1, "frame")
	status.StateVersion++
	second, _ := c.reference(status, 1, "frame")
	if first == second {
		t.Fatal("frame id reused after resume")
	}
	if _, err := c.nativeReference(status, first, "frame"); err == nil {
		t.Fatal("stale frame accepted")
	}
	other := &dapConnection{server: app, references: map[int]dapReference{}}
	third, _ := other.reference(status, 1, "frame")
	if third == second {
		t.Fatal("frame id reused after reconnect")
	}
	if _, err := other.nativeReference(status, second, "frame"); err == nil {
		t.Fatal("another connection's frame accepted")
	}
}
