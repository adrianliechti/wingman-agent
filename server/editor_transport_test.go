package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"go.lsp.dev/jsonrpc2"
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
	external := filepath.Join(t.TempDir(), "dependency.go")
	if err := os.WriteFile(external, []byte("package dependency\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unreported, _ := canonicalLSPSource(lspSourceScheme + strings.TrimPrefix(fileuri.FromPath(external), "file"))
	for _, uri := range []string{fileuri.FromPath(external), unreported} {
		if response := call(8, "workspace/textDocumentContent", map[string]any{"uri": uri}); response["error"] == nil {
			t.Fatalf("unreported external file was exposed as %s", uri)
		}
	}
	source, ok := app.lspSource(fileuri.FromPath(external))
	if !ok || source != unreported {
		t.Fatalf("external source = %q, %v", source, ok)
	}
	if response := call(9, "workspace/textDocumentContent", map[string]any{"uri": source}); response["error"] != nil || response["result"].(map[string]any)["text"] != "package dependency\n" {
		t.Fatal(response)
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

func TestLSPWebSocketProtocolErrorsAndEmptyResults(t *testing.T) {
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\n\nfunc Disk() {}\n\nfunc use() { Disk() }\n")
	writeDebugTestFile(t, root, "notes.txt", "plain text\n")
	app := newDebugTestServer(t, root)
	conn, ctx := dialEditorTest(t, app, "/lsp")
	code := func(response map[string]any) any {
		t.Helper()
		failure, _ := response["error"].(map[string]any)
		if failure == nil {
			t.Fatalf("expected an error: %v", response)
		}
		return failure["code"]
	}
	next := func(raw string) map[string]any {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := wsjson.Read(ctx, conn, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	call := func(id float64, method string, params any) map[string]any {
		return editorTestReply(t, conn, ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	if response := next("{"); code(response) != float64(-32700) || response["id"] != nil {
		t.Fatalf("parse error: %v", response)
	}
	if response := next(`{"jsonrpc":"2.0","id":{"x":1},"method":"initialize"}`); code(response) != float64(-32600) || response["id"] != nil {
		t.Fatalf("object id: %v", response)
	}
	if response := next(`{"jsonrpc":"2.0"}`); code(response) != float64(-32600) {
		t.Fatalf("message without method or id: %v", response)
	}
	call(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	if response := call(2, "initialize", map[string]any{}); code(response) != float64(-32600) {
		t.Fatalf("second initialize: %v", response)
	}
	if response := call(3, "initialized", map[string]any{}); code(response) != float64(-32600) {
		t.Fatalf("initialized sent as a request: %v", response)
	}

	text := fileuri.FromPath(filepath.Join(root, "notes.txt"))
	for method, want := range map[string]string{"textDocument/formatting": "null", "textDocument/codeAction": "[]", "textDocument/inlayHint": "null"} {
		response := call(4, method, map[string]any{"textDocument": map[string]any{"uri": text}, "range": map[string]any{}, "options": map[string]any{"tabSize": 2, "insertSpaces": true}, "context": map[string]any{"diagnostics": []any{}}})
		if wire, _ := json.Marshal(response["result"]); response["error"] != nil || string(wire) != want {
			t.Fatalf("%s without a language server: %v", method, response)
		}
	}
	source := fileuri.FromPath(filepath.Join(root, "main.go"))
	if response := call(5, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": source}, "position": map[string]any{"line": 1, "character": 0}}); response["error"] != nil || response["result"] != nil {
		t.Fatalf("hover over a blank line: %v", response)
	}
	references := func(include bool) int {
		t.Helper()
		response := call(6, "textDocument/references", map[string]any{"textDocument": map[string]any{"uri": source}, "position": map[string]any{"line": 2, "character": 5}, "context": map[string]any{"includeDeclaration": include}})
		locations, ok := response["result"].([]any)
		if !ok {
			t.Fatalf("references: %v", response)
		}
		return len(locations)
	}
	if with, without := references(true), references(false); with != without+1 {
		t.Fatalf("includeDeclaration ignored: %d with, %d without", with, without)
	}
	call(7, "shutdown", nil)
	if response := call(8, "textDocument/hover", map[string]any{}); code(response) != float64(-32600) {
		t.Fatalf("request after shutdown: %v", response)
	}
}

func TestEditorSocketErrorCodes(t *testing.T) {
	socket := &editorSocket{ctx: t.Context(), writer: newWSClient(nil), pending: map[string]context.CancelCauseFunc{}}
	cancelled, done, _ := socket.begin("1", time.Minute)
	defer done()
	socket.cancelRequest("1")
	expired, finish, _ := socket.begin("2", time.Nanosecond)
	defer finish()
	<-expired.Done()
	for err, want := range map[error]float64{
		editorRequestError(cancelled, context.Canceled):                                      -32800,
		editorRequestError(expired, context.DeadlineExceeded):                                -32803,
		fmt.Errorf("rename: %w", &jsonrpc2.Error{Code: -32801, Message: "content modified"}): -32801,
		errors.New("failed"): -32603,
	} {
		socket.reply(json.RawMessage("1"), nil, err)
		var response struct {
			Error struct {
				Code float64 `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(<-socket.writer.outbox, &response); err != nil || response.Error.Code != want {
			t.Fatalf("%v: code %v, want %v", err, response.Error.Code, want)
		}
	}
}

func TestEditorSocketsRejectForeignOrigins(t *testing.T) {
	app := newDebugTestServer(t, t.TempDir())
	web := httptest.NewServer(app.checkInstance(app.mux))
	t.Cleanup(web.Close)
	for _, path := range []string{"/lsp", "/dap"} {
		for _, origin := range []string{"null", "https://client.example", "vscode-file://vscode-app"} {
			conn, _, err := websocket.Dial(t.Context(), strings.Replace(web.URL, "http:", "ws:", 1)+path+"?instance="+app.scope.InstanceID, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{origin}}})
			if err == nil {
				conn.CloseNow()
				t.Fatalf("%s accepted origin %s", path, origin)
			}
		}
	}
}

func TestLegacyProtocolRoutesAreRemoved(t *testing.T) {
	app := newDebugTestServer(t, t.TempDir())
	for _, route := range []string{
		"GET /api/capabilities", "GET /api/v2/bootstrap", "GET /api/terminals/shells",
		"GET /api/files", "POST /api/files", "DELETE /api/files", "GET /api/files/read", "GET /api/files/search", "POST /api/files/content-search", "POST /api/files/write", "POST /api/files/write-batch", "POST /api/files/rename", "POST /api/files/copy", "GET /api/files/path", "POST /api/files/reveal", "GET /api/files/download", "GET /api/files/preview", "GET /api/lsp/file",

		"GET /api/git/commits", "POST /api/git/commits", "GET /api/git/branches", "POST /api/git/branches", "GET /api/git/diff",
		"POST /api/git/init", "POST /api/git/checkout", "POST /api/git/fetch", "POST /api/git/pull", "POST /api/git/push",
		"GET /api/git/status", "GET /api/git/history", "GET /api/git/insights", "GET /api/git/compare",
		"POST /api/git/stage", "POST /api/git/unstage", "POST /api/git/commit", "POST /api/git/commit-message",
		"GET /api/diffs", "POST /api/diffs/revert",
		"POST /api/editor/tab", "POST /api/editor/transform",
		"POST /api/settings/editor.tab.completion",
		"GET /api/v2/events",
		"GET /api/v2/backends/one/sessions",
		"POST /api/v2/backends/one/sessions",
		"POST /api/v2/backends/one/sessions/saved/commands",
		"GET /api/terminals", "POST /api/terminals",
		"POST /api/lsp/hover", "POST /api/lsp/document",
		"POST /api/debug/targets", "POST /api/debug/plan", "GET /api/skills",
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
