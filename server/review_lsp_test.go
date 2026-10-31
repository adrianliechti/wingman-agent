package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// reviewLSPFakeSource is a minimal stdio language server. Hover answers with
// the text it holds for the document, so tests see exactly what the gateway
// synchronized. Flag files in $REVIEW_LSP_FAKE steer it: "hold" delays the
// initialize response until "release" exists; "wedge" stops reading stdin
// after initialized.
const reviewLSPFakeSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type message struct {
	ID     json.RawMessage ` + "`json:\"id,omitempty\"`" + `
	Method string          ` + "`json:\"method,omitempty\"`" + `
	Params json.RawMessage ` + "`json:\"params,omitempty\"`" + `
	Result json.RawMessage ` + "`json:\"result,omitempty\"`" + `
}

var (
	writeMu  sync.Mutex
	stateMu  sync.Mutex
	texts    = map[string]string{}
	versions = map[string]int{}
	cancels  = map[string]chan struct{}{}
	replies  = map[string]chan message{}
	control  = os.Getenv("REVIEW_LSP_FAKE")
)

func send(v any) {
	data, _ := json.Marshal(v)
	writeMu.Lock()
	defer writeMu.Unlock()
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
}

func flag(name string) bool {
	_, err := os.Stat(filepath.Join(control, name))
	return err == nil
}

func main() {
	_ = os.WriteFile(filepath.Join(control, "pid-"+strconv.Itoa(os.Getpid())), nil, 0o644)
	reader := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		length := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				os.Exit(0)
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			os.Exit(0)
		}
		var m message
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		handle(m)
		if m.Method == "initialized" && flag("wedge") {
			for {
				time.Sleep(time.Hour)
			}
		}
	}
}

func handle(m message) {
	if m.Method == "" {
		stateMu.Lock()
		ch := replies[string(m.ID)]
		stateMu.Unlock()
		if ch != nil {
			ch <- m
		}
		return
	}
	reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
	fail := func(code int, text string) {
		send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": code, "message": text}})
	}
	var p struct {
		TextDocument struct {
			URI     string ` + "`json:\"uri\"`" + `
			Text    string ` + "`json:\"text\"`" + `
			Version int    ` + "`json:\"version\"`" + `
		} ` + "`json:\"textDocument\"`" + `
		ContentChanges []struct {
			Text string ` + "`json:\"text\"`" + `
		} ` + "`json:\"contentChanges\"`" + `
		ID        json.RawMessage ` + "`json:\"id\"`" + `
		Arguments []string        ` + "`json:\"arguments\"`" + `
	}
	_ = json.Unmarshal(m.Params, &p)
	uri := p.TextDocument.URI
	zero := map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 0}}
	switch m.Method {
	case "initialize":
		for flag("hold") && !flag("release") {
			time.Sleep(5 * time.Millisecond)
		}
		reply(map[string]any{"capabilities": map[string]any{
			"textDocumentSync":       map[string]any{"openClose": true, "change": 1},
			"hoverProvider":          true,
			"renameProvider":         true,
			"executeCommandProvider": map[string]any{"commands": []string{"review.edit"}},
		}})
	case "shutdown":
		reply(nil)
	case "exit":
		os.Exit(0)
	case "textDocument/didOpen":
		stateMu.Lock()
		texts[uri], versions[uri] = p.TextDocument.Text, p.TextDocument.Version
		stateMu.Unlock()
	case "textDocument/didChange":
		stateMu.Lock()
		for _, change := range p.ContentChanges {
			texts[uri] = change.Text
		}
		versions[uri] = p.TextDocument.Version
		stateMu.Unlock()
	case "textDocument/didClose":
		stateMu.Lock()
		delete(texts, uri)
		delete(versions, uri)
		stateMu.Unlock()
	case "$/cancelRequest":
		stateMu.Lock()
		ch := cancels[string(p.ID)]
		delete(cancels, string(p.ID))
		stateMu.Unlock()
		if ch != nil {
			close(ch)
		}
	case "textDocument/hover":
		stateMu.Lock()
		text, open := texts[uri]
		stateMu.Unlock()
		if !open {
			text = "<closed>"
		}
		if strings.Contains(text, "SLOW") {
			ch := make(chan struct{})
			stateMu.Lock()
			cancels[string(m.ID)] = ch
			stateMu.Unlock()
			go func() {
				<-ch
				fail(-32800, "cancelled")
			}()
			return
		}
		reply(map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "text:" + text}})
	case "textDocument/rename":
		stateMu.Lock()
		text, version := texts[uri], versions[uri]
		stateMu.Unlock()
		reply(map[string]any{"documentChanges": []any{map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": version},
			"edits":        []any{map[string]any{"range": zero, "newText": text}},
		}}})
	case "workspace/executeCommand":
		go func() {
			key, _ := json.Marshal("edit-" + string(m.ID))
			ch := make(chan message, 1)
			stateMu.Lock()
			replies[string(key)] = ch
			stateMu.Unlock()
			target := ""
			if len(p.Arguments) > 0 {
				target = p.Arguments[0]
			}
			send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(key), "method": "workspace/applyEdit", "params": map[string]any{
				"label": "review", "edit": map[string]any{"changes": map[string]any{target: []any{map[string]any{"range": zero, "newText": "// edited\n"}}}},
			}})
			response := <-ch
			var result struct {
				Applied bool ` + "`json:\"applied\"`" + `
			}
			_ = json.Unmarshal(response.Result, &result)
			reply(map[string]any{"applied": result.Applied})
		}()
	default:
		if len(m.ID) > 0 {
			fail(-32601, "unsupported")
		}
	}
}
`

// reviewLSPInstallFake installs the fake as Wingman's managed gopls. Call it
// before the app is created: server detection is cached.
func reviewLSPInstallFake(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go toolchain is needed to build the fake language server")
	}
	home := testenv.WingmanHome(t)
	source := t.TempDir()
	for name, content := range map[string]string{"go.mod": "module reviewlspfake\n\ngo 1.21\n", "main.go": reviewLSPFakeSource} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(home, "tools", "gopls", "bin", "gopls")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command(goTool, "build", "-o", binary, ".")
	build.Dir = source
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake language server: %v\n%s", err, output)
	}
	control := t.TempDir()
	t.Setenv("REVIEW_LSP_FAKE", control)
	t.Cleanup(func() { reviewLSPKillFakes(control) })
	return control
}

func reviewLSPFakePIDs(control string) []int {
	entries, _ := os.ReadDir(control)
	var pids []int
	for _, entry := range entries {
		if pid, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "pid-")); err == nil && strings.HasPrefix(entry.Name(), "pid-") {
			pids = append(pids, pid)
		}
	}
	return pids
}

func reviewLSPKillFakes(control string) {
	for _, pid := range reviewLSPFakePIDs(control) {
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}
}

func reviewLSPFlag(t *testing.T, control, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(control, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// reviewLSPPeer reads in the background so a timed wait never cancels a
// WebSocket read, which would close the connection.
type reviewLSPPeer struct {
	t        *testing.T
	conn     *websocket.Conn
	ctx      context.Context
	root     string
	messages chan map[string]any
	closed   chan struct{}
}

func reviewLSPDial(t *testing.T, app *Server, root string, capabilities map[string]any) *reviewLSPPeer {
	t.Helper()
	conn, ctx := dialEditorTest(t, app, "/lsp")
	conn.SetReadLimit(16 << 20)
	p := &reviewLSPPeer{t: t, conn: conn, ctx: ctx, root: root, messages: make(chan map[string]any, 4096), closed: make(chan struct{})}
	go func() {
		defer close(p.closed)
		for {
			var message map[string]any
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				return
			}
			select {
			case p.messages <- message:
			case <-ctx.Done():
				return
			}
		}
	}()
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	if response := p.call(-1, "initialize", map[string]any{"capabilities": capabilities}); response["error"] != nil {
		t.Fatalf("initialize: %v", response)
	}
	p.notify("initialized", map[string]any{})
	return p
}

func (p *reviewLSPPeer) uri(name string) string {
	return fileuri.FromPath(filepath.Join(p.root, filepath.FromSlash(name)))
}

func (p *reviewLSPPeer) write(value any) error { return wsjson.Write(p.ctx, p.conn, value) }

func (p *reviewLSPPeer) notify(method string, params any) {
	p.t.Helper()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
}

func (p *reviewLSPPeer) request(id float64, method string, params any) {
	p.t.Helper()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
}

// next returns the first message that matches, dropping others.
func (p *reviewLSPPeer) next(timeout time.Duration, match func(map[string]any) bool) (map[string]any, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case message := <-p.messages:
			if match(message) {
				return message, true
			}
		case <-p.closed:
			select {
			case message := <-p.messages:
				if match(message) {
					return message, true
				}
				continue
			default:
			}
			return nil, false
		case <-timer.C:
			return nil, false
		}
	}
}

func (p *reviewLSPPeer) response(id float64, timeout time.Duration) (map[string]any, bool) {
	return p.next(timeout, func(message map[string]any) bool { return message["method"] == nil && message["id"] == id })
}

func (p *reviewLSPPeer) call(id float64, method string, params any) map[string]any {
	p.t.Helper()
	p.request(id, method, params)
	response, ok := p.response(id, 10*time.Second)
	if !ok {
		p.t.Fatalf("%s %v: no response", method, id)
	}
	return response
}

func (p *reviewLSPPeer) open(name, text string, version int) {
	p.t.Helper()
	p.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": p.uri(name), "languageId": "go", "version": version, "text": text}})
}

func (p *reviewLSPPeer) change(name, text string, version int) {
	p.t.Helper()
	p.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": p.uri(name), "version": version}, "contentChanges": []any{map[string]any{"text": text}}})
}

func (p *reviewLSPPeer) closeDocument(name string) {
	p.t.Helper()
	p.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": p.uri(name)}})
}

func (p *reviewLSPPeer) hover(id float64, name string) string {
	p.t.Helper()
	response := p.call(id, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": p.uri(name)}, "position": map[string]any{"line": 0, "character": 0}})
	if response["error"] != nil {
		p.t.Fatalf("hover %s: %v", name, response["error"])
	}
	result, _ := response["result"].(map[string]any)
	contents, _ := result["contents"].(map[string]any)
	value, _ := contents["value"].(string)
	return value
}

// barrier returns once every earlier notification on this connection has been
// applied: requests start only after the notifications queued before them.
func (p *reviewLSPPeer) barrier(id float64) {
	p.t.Helper()
	p.call(id, "workspace/textDocumentContent", map[string]any{"uri": "wingman-source:/review-barrier"})
}

func reviewLSPErrorCode(message map[string]any) any {
	failure, _ := message["error"].(map[string]any)
	return failure["code"]
}

const reviewLSPGoMod = "module review\n\ngo 1.24\n"

// Bug: when one connection closes a document that another connection still has
// open, the language server keeps the closing connection's discarded buffer and
// the remaining editor's requests are answered against it. The remaining
// connection's buffer should be resynchronized.
func TestReviewLSPClosingSharedDocumentKeepsTheRemainingBuffer(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	a := reviewLSPDial(t, app, root, nil)
	b := reviewLSPDial(t, app, root, nil)

	a.open("main.go", "package main // tab A\n", 1)
	if got := a.hover(1, "main.go"); got != "text:package main // tab A\n" {
		t.Fatalf("tab A hover = %q", got)
	}
	b.open("main.go", "package main // tab B, discarded\n", 1)
	b.barrier(2)
	b.closeDocument("main.go")
	b.barrier(3)

	if got := a.hover(4, "main.go"); got != "text:package main // tab A\n" {
		t.Fatalf("tab A's request was answered against %q, a buffer tab B closed without saving", got)
	}
}

// Bug: two connections editing the same file share one language-server
// document, so a rename computed for the other tab's text is labeled with this
// tab's version and would be applied to this tab's buffer.
func TestReviewLSPRenameUsesTheRequestingConnectionsBuffer(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	a := reviewLSPDial(t, app, root, nil)
	b := reviewLSPDial(t, app, root, nil)

	b.open("main.go", "package main\n\nfunc fromB() {}\n", 7)
	b.barrier(1)
	a.open("main.go", "package main\n\n\n\nfunc fromA() {}\n", 1)
	a.barrier(2)

	response := b.call(3, "textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": b.uri("main.go")}, "position": map[string]any{"line": 2, "character": 6}, "newName": "renamed"})
	var edit struct {
		DocumentChanges []struct {
			TextDocument struct {
				Version *int `json:"version"`
			} `json:"textDocument"`
			Edits []struct {
				NewText string `json:"newText"`
			} `json:"edits"`
		} `json:"documentChanges"`
	}
	data, _ := json.Marshal(response["result"])
	if err := json.Unmarshal(data, &edit); err != nil || len(edit.DocumentChanges) != 1 || len(edit.DocumentChanges[0].Edits) != 1 {
		t.Fatalf("rename = %s, %v", data, err)
	}
	change := edit.DocumentChanges[0]
	if change.TextDocument.Version == nil || *change.TextDocument.Version != 7 {
		t.Fatalf("edit version = %v, want tab B's 7", change.TextDocument.Version)
	}
	// The fake server's edit carries the text it computed the rename for.
	if computed := change.Edits[0].NewText; !strings.Contains(computed, "fromB") {
		t.Fatalf("an edit labeled with tab B's version 7 was computed for %q (tab A's buffer)", computed)
	}
}

// Bug: a request cancelled while it waits behind a document notification is
// answered -32603 (the bare context error) instead of RequestCancelled -32800.
func TestReviewLSPQueuedRequestCancellationAnswersRequestCancelled(t *testing.T) {
	control := reviewLSPInstallFake(t)
	reviewLSPFlag(t, control, "hold")
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)

	// didOpen waits for the language server to start.
	c.open("main.go", "package main // unsaved\n", 1)
	c.request(10, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "position": map[string]any{"line": 0, "character": 0}})
	c.notify("$/cancelRequest", map[string]any{"id": 10})
	// The read loop answers a parse error itself, after it has handled the cancel.
	if err := c.conn.Write(c.ctx, websocket.MessageText, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.next(5*time.Second, func(m map[string]any) bool { return reviewLSPErrorCode(m) == float64(-32700) }); !ok {
		t.Fatal("no parse error response")
	}
	reviewLSPFlag(t, control, "release")
	response, ok := c.response(10, 10*time.Second)
	if !ok {
		t.Fatal("cancelled request was never answered")
	}
	if code := reviewLSPErrorCode(response); code != float64(-32800) {
		t.Fatalf("cancelled queued request answered %v, want -32800: %v", code, response)
	}
}

// Bug: while a document notification waits for a cold language server, more
// than 128 queued messages (fast typing) make the gateway drop the connection.
// The server being slow is not the client's fault; updates should be kept or
// coalesced and the connection kept.
func TestReviewLSPTypingDuringColdStartKeepsTheConnection(t *testing.T) {
	control := reviewLSPInstallFake(t)
	reviewLSPFlag(t, control, "hold")
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)

	c.open("main.go", "package main\n", 1)
	last := 0
	for version := 2; version <= 300; version++ {
		text := fmt.Sprintf("package main // keystroke %d\n", version)
		if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{
			"textDocument": map[string]any{"uri": c.uri("main.go"), "version": version}, "contentChanges": []any{map[string]any{"text": text}},
		}}); err != nil {
			break
		}
		last = version
	}
	reviewLSPFlag(t, control, "release")
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "textDocument/hover", "params": map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "position": map[string]any{"line": 0, "character": 0}}}); err != nil {
		t.Fatalf("connection dropped after %d keystrokes during a cold start: %v", last, err)
	}
	response, ok := c.response(99, 10*time.Second)
	if !ok {
		t.Fatalf("connection dropped after %d keystrokes during a cold start", last)
	}
	result, _ := response["result"].(map[string]any)
	contents, _ := result["contents"].(map[string]any)
	if want := "text:package main // keystroke 300\n"; contents["value"] != want {
		t.Fatalf("hover = %v, want %q", response, want)
	}
}

// Bug: releaseDocument holds the gateway-wide lspDocumentsMu while it waits on
// the language server, so one wedged server stalls document sync on every
// connection (even for files no server covers) and the workspace poller's
// refreshDiagnostics.
func TestReviewLSPWedgedServerDoesNotStallOtherConnections(t *testing.T) {
	control := reviewLSPInstallFake(t)
	reviewLSPFlag(t, control, "wedge")
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n", "notes.txt": "plain\n"})
	t.Cleanup(func() { reviewLSPKillFakes(control) }) // before app.Close
	a := reviewLSPDial(t, app, root, nil)
	b := reviewLSPDial(t, app, root, nil)
	c := reviewLSPDial(t, app, root, nil)

	// Small messages still fit the pipe of a server that stopped reading.
	b.open("main.go", "package main // b\n", 1)
	b.barrier(1)
	// A large buffer blocks inside the language server write.
	a.open("big.go", "package main\n// "+strings.Repeat("x", 4<<20)+"\n", 1)
	a.request(2, "workspace/textDocumentContent", map[string]any{"uri": "wingman-source:/review-barrier"})
	if _, ok := a.response(2, time.Second); ok {
		t.Fatal("the large didOpen did not block; the test setup is invalid")
	}
	b.closeDocument("main.go")
	b.request(3, "workspace/textDocumentContent", map[string]any{"uri": "wingman-source:/review-barrier"})
	if _, ok := b.response(3, 500*time.Millisecond); ok {
		t.Fatal("closing the document did not wait for the server; the test setup is invalid")
	}

	refreshed := make(chan struct{})
	go func() {
		app.refreshDiagnostics()
		close(refreshed)
	}()
	c.open("notes.txt", "plain but unsaved\n", 1)
	c.request(4, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": c.uri("notes.txt")}})
	if _, ok := c.response(4, 3*time.Second); !ok {
		t.Error("a file without a language server stalled behind another connection's wedged server")
	}
	select {
	case <-refreshed:
	case <-time.After(3 * time.Second):
		t.Error("refreshDiagnostics stalled behind a wedged language server")
	}
}

// Bug: a document closed while its language server is down is restored, as an
// editor-owned buffer, into the restarted server. The closed tab's unsaved
// text then shadows the file on disk for every later request.
func TestReviewLSPDocumentClosedDuringRestartIsNotRestored(t *testing.T) {
	control := reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main // disk\n", "other.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	c.open("main.go", "package main // discarded\n", 1)
	if got := c.hover(1, "main.go"); got != "text:package main // discarded\n" {
		t.Fatalf("hover = %q", got)
	}
	reviewLSPKillFakes(control)
	for deadline := time.Now().Add(10 * time.Second); len(app.workspace.LSPActivities()) != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the killed language server still reports as alive")
		}
	}
	c.closeDocument("main.go")
	c.barrier(2)
	// Any request restarts the server.
	c.hover(3, "other.go")
	if got := c.hover(4, "main.go"); got != "text:package main // disk\n" {
		t.Fatalf("after the editor closed main.go, the restarted server still has %q", got)
	}
}

// Bug: workspace/executeCommand waits for another command's review on a mutex
// that ignores cancellation, so $/cancelRequest gets no answer until a review
// in some other tab ends.
func TestReviewLSPCommandWaitingForAnotherReviewCanBeCancelled(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	a := reviewLSPDial(t, app, root, nil)
	b := reviewLSPDial(t, app, root, nil)
	command := func(p *reviewLSPPeer, id float64) {
		p.request(id, "workspace/executeCommand", map[string]any{"textDocument": map[string]any{"uri": p.uri("main.go")}, "command": "review.edit", "arguments": []any{p.uri("main.go")}})
	}
	command(a, 1)
	review, ok := a.next(10*time.Second, func(m map[string]any) bool { return m["method"] == "workspace/applyEdit" })
	if !ok {
		t.Fatal("tab A was not asked to review the edit")
	}
	defer func() {
		_ = a.write(map[string]any{"jsonrpc": "2.0", "id": review["id"], "result": map[string]any{"applied": false}})
	}()

	command(b, 2)
	time.Sleep(200 * time.Millisecond)
	b.notify("$/cancelRequest", map[string]any{"id": 2})
	response, ok := b.response(2, 3*time.Second)
	if !ok {
		t.Fatal("tab B's cancelled command got no answer while tab A's review was open")
	}
	if code := reviewLSPErrorCode(response); code != float64(-32800) {
		t.Fatalf("cancelled command answered %v, want -32800", code)
	}
}

// Bug: the gateway answers a JSON-RPC response that has a null id (a client's
// error response) with an Invalid Request error; responses must never be answered.
func TestReviewLSPDoesNotAnswerResponses(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	if err := c.conn.Write(c.ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"client could not parse a message"}}`)); err != nil {
		t.Fatal(err)
	}
	c.request(1, "wingman/documentCapabilities", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}})
	message, ok := c.next(5*time.Second, func(map[string]any) bool { return true })
	if !ok || message["id"] != float64(1) {
		t.Fatalf("the gateway answered a response: %v", message)
	}
}

// Bug: resolve requests are decoded into lspFeatureParams, whose Command is a
// string, so a CompletionItem or CodeAction carrying its LSP Command object is
// rejected with -32602 instead of being resolved.
func TestReviewLSPResolvesItemsThatCarryACommand(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	route := map[string]any{"wingman": map[string]any{"uri": c.uri("main.go"), "data": map[string]any{"id": 1}}}
	command := map[string]any{"title": "Trigger parameter hints", "command": "editor.action.triggerParameterHints"}
	for id, call := range map[float64]struct {
		method string
		item   map[string]any
	}{
		1: {"completionItem/resolve", map[string]any{"label": "greet", "command": command, "data": route}},
		2: {"codeAction/resolve", map[string]any{"title": "Organize imports", "kind": "source.organizeImports", "command": command, "data": route}},
	} {
		response := c.call(id, call.method, call.item)
		if response["error"] != nil {
			t.Errorf("%s of an item with a command: %v", call.method, response["error"])
			continue
		}
		result, _ := response["result"].(map[string]any)
		if inner, _ := result["command"].(map[string]any); inner["command"] != command["command"] {
			t.Errorf("%s lost the item's command: %v", call.method, result)
		}
	}
}

// Hard tests (expected to pass).

func TestReviewLSPCancellingASlowNativeRequest(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	c.open("main.go", "package main // SLOW\n", 1)
	// Queued requests are covered by TestReviewLSPQueuedRequestCancellationAnswersRequestCancelled.
	c.barrier(1)
	for round := range 3 {
		c.request(7, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "position": map[string]any{"line": 0, "character": 0}})
		if _, ok := c.response(7, 200*time.Millisecond); ok {
			t.Fatal("slow hover answered before cancellation")
		}
		c.notify("$/cancelRequest", map[string]any{"id": 7})
		response, ok := c.response(7, 5*time.Second)
		if !ok || reviewLSPErrorCode(response) != float64(-32800) {
			t.Fatalf("round %d: cancelled hover = %v", round, response)
		}
	}
	// Cancelling unknown or finished ids is harmless.
	c.notify("$/cancelRequest", map[string]any{"id": 7})
	c.notify("$/cancelRequest", map[string]any{"id": "missing"})
	c.change("main.go", "package main // fast\n", 2)
	if got := c.hover(8, "main.go"); got != "text:package main // fast\n" {
		t.Fatalf("hover after cancellations = %q", got)
	}
}

func TestReviewLSPRapidChangesReachTheServerInOrder(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	c.open("main.go", "package main\n", 1)
	if got := c.hover(1, "main.go"); got != "text:package main\n" {
		t.Fatalf("hover = %q", got)
	}
	for round := range 5 {
		base := 2 + round*40
		for version := base; version < base+40; version++ {
			c.change("main.go", fmt.Sprintf("package main // v%d\n", version), version)
		}
		// Stale and repeated versions are rejected and reported.
		c.change("main.go", "package main // stale\n", base)
		c.change("main.go", "package main // stale\n", 1)
		for range 2 {
			if _, ok := c.next(5*time.Second, func(m map[string]any) bool { return m["method"] == "window/logMessage" }); !ok {
				t.Fatalf("round %d: a stale version was not reported", round)
			}
		}
		want := fmt.Sprintf("text:package main // v%d\n", base+39)
		if got := c.hover(float64(100+round), "main.go"); got != want {
			t.Fatalf("round %d: hover = %q, want %q", round, got, want)
		}
	}
}

func TestReviewLSPDocumentsAreReleasedWhenTheLastConnectionLeaves(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main // disk\n"})
	path := filepath.Join(root, "main.go")
	a := reviewLSPDial(t, app, root, nil)
	b := reviewLSPDial(t, app, root, nil)
	a.open("main.go", "package main // shared\n", 1)
	a.barrier(1)
	b.open("main.go", "package main // shared\n", 1)
	b.barrier(2)

	_ = a.conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	if content, open := app.workspace.EditorLSPDocumentContent(path); !open || content != "package main // shared\n" {
		t.Fatalf("closing one of two connections released the document: %q, %v", content, open)
	}
	_ = b.conn.Close(websocket.StatusNormalClosure, "")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		app.lspDocumentsMu.Lock()
		owners := len(app.lspDocuments)
		app.lspDocumentsMu.Unlock()
		if _, open := app.workspace.EditorLSPDocumentContent(path); !open && owners == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("documents stayed open after every connection left (%d owned paths)", owners)
		}
	}
}

func TestReviewLSPConcurrentConnectionsStress(t *testing.T) {
	reviewLSPInstallFake(t)
	files := map[string]string{"go.mod": reviewLSPGoMod}
	for i := range 4 {
		files[fmt.Sprintf("f%d.go", i)] = "package main\n"
	}
	files["notes.txt"] = "plain\n"
	app, root := newLSPTestApp(t, files)
	peers := make([]*reviewLSPPeer, 6)
	for i := range peers {
		peers[i] = reviewLSPDial(t, app, root, map[string]any{"workspace": map[string]any{"diagnostics": map[string]any{"refreshSupport": true}}})
	}
	var wait sync.WaitGroup
	for i, p := range peers {
		wait.Go(func() {
			id := float64(1000)
			for round := range 15 {
				name := fmt.Sprintf("f%d.go", (i+round)%4)
				if round%5 == 4 {
					name = "notes.txt"
				}
				text := fmt.Sprintf("package main // peer %d round %d\n", i, round)
				if p.write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{"uri": p.uri(name), "languageId": "go", "version": 1, "text": text}}}) != nil {
					return
				}
				for version := 2; version < 6; version++ {
					text = fmt.Sprintf("package main // peer %d round %d v%d\n", i, round, version)
					_ = p.write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{"textDocument": map[string]any{"uri": p.uri(name), "version": version}, "contentChanges": []any{map[string]any{"text": text}}}})
				}
				id++
				_ = p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "textDocument/documentSymbol", "params": map[string]any{"textDocument": map[string]any{"uri": p.uri(name)}}})
				if round%3 == 0 {
					id++
					_ = p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "workspace/diagnostic", "params": map[string]any{"previousResultIds": []any{}}})
					go app.refreshDiagnostics()
				}
				if _, ok := p.response(id, 10*time.Second); !ok {
					t.Errorf("peer %d round %d: no response", i, round)
					return
				}
				_ = p.write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didClose", "params": map[string]any{"textDocument": map[string]any{"uri": p.uri(name)}}})
			}
			if i%2 == 0 {
				_ = p.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "shutdown"})
				if response, ok := p.response(1, 10*time.Second); !ok || response["error"] != nil {
					t.Errorf("peer %d shutdown = %v", i, response)
				}
				_ = p.write(map[string]any{"jsonrpc": "2.0", "method": "exit"})
			} else {
				_ = p.conn.CloseNow()
			}
		})
	}
	wait.Wait()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		app.lspDocumentsMu.Lock()
		owners, refreshers := len(app.lspDocuments), len(app.lspRefreshers)
		app.lspDocumentsMu.Unlock()
		open := 0
		for i := range 4 {
			if _, ok := app.workspace.EditorLSPDocumentContent(filepath.Join(root, fmt.Sprintf("f%d.go", i))); ok {
				open++
			}
		}
		if owners == 0 && refreshers == 0 && open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after all connections left: %d owned paths, %d refreshers, %d open documents", owners, refreshers, open)
		}
	}
}

func TestReviewLSPCommandEditReviewRoundTrip(t *testing.T) {
	reviewLSPInstallFake(t)
	app, root := newLSPTestApp(t, map[string]string{"go.mod": reviewLSPGoMod, "main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	c.open("main.go", "package main // unsaved\n", 3)
	for id, answer := range map[float64]map[string]any{
		1: {"result": map[string]any{"applied": true}},
		2: {"result": map[string]any{"applied": false}},
		3: {"error": map[string]any{"code": -32603, "message": "review failed"}},
	} {
		c.request(id, "workspace/executeCommand", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "command": "review.edit", "arguments": []any{c.uri("main.go")}})
		review, ok := c.next(10*time.Second, func(m map[string]any) bool { return m["method"] == "workspace/applyEdit" })
		if !ok {
			t.Fatalf("command %v: no review", id)
		}
		params, _ := review["params"].(map[string]any)
		if params["_wingmanRequestId"] != id {
			t.Fatalf("review is not tied to its command: %v", params)
		}
		// A reply to an unknown id is ignored.
		if err := c.write(map[string]any{"jsonrpc": "2.0", "id": "apply:999999", "result": map[string]any{"applied": true}}); err != nil {
			t.Fatal(err)
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": review["id"]}
		for key, value := range answer {
			reply[key] = value
		}
		if err := c.write(reply); err != nil {
			t.Fatal(err)
		}
		response, ok := c.response(id, 10*time.Second)
		if !ok || response["error"] != nil {
			t.Fatalf("command %v = %v", id, response)
		}
		applied := response["result"].(map[string]any)["applied"]
		if want := id == 1; applied != want {
			t.Fatalf("command %v reported applied=%v, want %v", id, applied, want)
		}
	}
	// A command the server does not provide is refused before reaching it.
	if response := c.call(4, "workspace/executeCommand", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "command": "review.other"}); response["error"] == nil {
		t.Fatalf("unknown command accepted: %v", response)
	}
}

func TestReviewLSPLifecycleEdges(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n\nfunc Disk() {}\n"})
	conn, ctx := dialEditorTest(t, app, "/lsp")
	p := &reviewLSPPeer{t: t, conn: conn, ctx: ctx, root: root, messages: make(chan map[string]any, 64), closed: make(chan struct{})}
	go func() {
		defer close(p.closed)
		for {
			var message map[string]any
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				return
			}
			p.messages <- message
		}
	}()
	// Notifications before initialize are dropped, not applied later.
	p.open("main.go", "package main\n\nfunc Early() {}\n", 1)
	if response := p.call(1, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": p.uri("main.go")}}); reviewLSPErrorCode(response) != float64(-32002) {
		t.Fatalf("request before initialize: %v", response)
	}
	if response := p.call(2, "initialize", map[string]any{"capabilities": map[string]any{}}); response["error"] != nil {
		t.Fatal(response)
	}
	p.notify("initialized", map[string]any{})
	wire, _ := json.Marshal(p.call(3, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": p.uri("main.go")}}))
	if !strings.Contains(string(wire), "Disk") || strings.Contains(string(wire), "Early") {
		t.Fatalf("a didOpen sent before initialize was applied: %s", wire)
	}
	if response := p.call(4, "$/wingman/unknown", nil); reviewLSPErrorCode(response) != float64(-32601) {
		t.Fatalf("$/ request: %v", response)
	}
	if response := p.call(5, "shutdown", nil); response["error"] != nil {
		t.Fatal(response)
	}
	if response := p.call(6, "textDocument/hover", map[string]any{}); reviewLSPErrorCode(response) != float64(-32600) {
		t.Fatalf("request after shutdown: %v", response)
	}
	p.notify("exit", nil)
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("exit did not close the connection")
	}
}

func TestReviewLSPResolveDataTampering(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n\nfunc greet() {}\n\nfunc main() { greet() }\n"})
	c := reviewLSPDial(t, app, root, nil)
	if err := os.Mkdir(filepath.Join(root, "dir.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.go")
	id := float64(10)
	for name, data := range map[string]any{
		"missing":        nil,
		"string":         "x",
		"array":          []any{1},
		"no wingman":     map[string]any{"uri": c.uri("main.go")},
		"wingman string": map[string]any{"wingman": "x"},
		"empty uri":      map[string]any{"wingman": map[string]any{"uri": ""}},
		"outside":        map[string]any{"wingman": map[string]any{"uri": fileuri.FromPath(outside)}},
		"traversal":      map[string]any{"wingman": map[string]any{"uri": c.uri("main.go") + "/../../../etc/hosts"}},
		"foreign scheme": map[string]any{"wingman": map[string]any{"uri": "untitled:main.go"}},
		"source scheme":  map[string]any{"wingman": map[string]any{"uri": "wingman-source:/etc/hosts"}},
		"directory":      map[string]any{"wingman": map[string]any{"uri": c.uri("dir.go")}},
	} {
		for _, method := range []string{"completionItem/resolve", "codeAction/resolve"} {
			id++
			item := map[string]any{"label": "greet", "title": "greet"}
			if data != nil {
				item["data"] = data
			}
			if response := c.call(id, method, item); reviewLSPErrorCode(response) == nil {
				t.Errorf("%s with %s data was accepted: %v", method, name, response)
			}
		}
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	response := c.call(1, "textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "position": map[string]any{"line": 4, "character": 14}})
	data, _ := json.Marshal(response["result"])
	if err := json.Unmarshal(data, &list); err != nil || len(list.Items) == 0 {
		t.Fatalf("completion = %s", data)
	}
	item := list.Items[0]
	// A client may substitute its own payload under the routing envelope.
	item["data"].(map[string]any)["wingman"].(map[string]any)["data"] = map[string]any{"client": strings.Repeat("y", 1024)}
	resolved := c.call(2, "completionItem/resolve", item)
	result, _ := resolved["result"].(map[string]any)
	if resolved["error"] != nil || result["label"] != item["label"] {
		t.Fatalf("resolve = %v", resolved)
	}
	if inner, _ := result["data"].(map[string]any); inner["client"] == nil {
		t.Fatalf("resolve did not return the item's own data: %v", result["data"])
	}
}

func TestReviewLSPExternalSourceURIs(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	external := filepath.Join(t.TempDir(), "dep dir", "a#b%c?d ü+@.go")
	if err := os.MkdirAll(filepath.Dir(external), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("package dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(external), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, ok := app.lspSource(fileuri.FromPath(external))
	if !ok {
		t.Fatal("external file was not mapped")
	}
	var content struct {
		Text string `json:"text"`
	}
	data, _ := json.Marshal(c.call(1, "workspace/textDocumentContent", map[string]any{"uri": source})["result"])
	if err := json.Unmarshal(data, &content); err != nil || content.Text != "package dep\n" {
		t.Fatalf("content of %s = %s", source, data)
	}
	virtual, ok := app.lspSource("jdt://contents/rt.jar/java.lang/String.class?=project/java.lang.String")
	if !ok {
		t.Fatal("virtual source was not mapped")
	}
	dir := strings.TrimSuffix(source, source[strings.LastIndex(source, "/"):])
	id := float64(10)
	for _, forged := range []string{
		dir + "/secret.txt",
		dir + "/../" + filepath.Base(filepath.Dir(external)) + "/secret.txt",
		dir + "/%2e%2e/" + filepath.Base(filepath.Dir(external)) + "/secret.txt",
		source + "?x=1",
		source + "/..",
		strings.Replace(source, "wingman-source:", "WINGMAN-SOURCE:", 1),
		strings.Replace(source, "wingman-source:", "file:", 1),
		strings.Replace(virtual, "/String.class", "/../secret.txt", 1),
		"wingman-source://file/" + "ZmlsZTovLy9ldGMvaG9zdHM" + "/hosts",
		"wingman-source:",
		"wingman-source:%",
		"",
	} {
		id++
		if response := c.call(id, "workspace/textDocumentContent", map[string]any{"uri": forged}); response["error"] == nil {
			t.Errorf("forged source %q was served: %v", forged, response["result"])
		}
	}
	// Workspace files are never served as external sources.
	if _, ok := app.lspSource(c.uri("main.go")); ok {
		t.Fatal("a workspace file was mapped to a source URI")
	}
	if _, ok := app.lspSource(fileuri.FromPath(filepath.Dir(external))); ok {
		t.Fatal("a directory was mapped to a source URI")
	}
}

func TestReviewLSPDocumentURIConfinement(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"link.go": filepath.Join(outside, "secret.go"), "linkdir": outside}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "dir.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := reviewLSPDial(t, app, root, nil)
	base := fileuri.FromPath(root)
	escapes := []string{
		base + "/../" + filepath.Base(outside) + "/secret.go",
		base + "/%2e%2e/" + filepath.Base(outside) + "/secret.go",
		base + "/sub%2F..%2F..%2F" + filepath.Base(outside) + "%2Fsecret.go",
		base + "/link.go",
		base + "/linkdir/secret.go",
		base + "/linkdir/new.go",
		base + "/dir.go",
		base + "/missing/new.go",
		fileuri.FromPath(filepath.Join(outside, "secret.go")),
		"file://localhost" + strings.TrimPrefix(fileuri.FromPath(filepath.Join(outside, "secret.go")), "file://"),
		"file:main.go",
		"untitled:" + filepath.Join(root, "main.go"),
		lspSourceScheme + ":" + strings.TrimPrefix(base, "file:") + "/main.go",
		"",
	}
	id := float64(10)
	for _, uri := range escapes {
		c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": "package injected\n"}})
		if _, ok := c.next(5*time.Second, func(m map[string]any) bool { return m["method"] == "window/logMessage" }); !ok {
			t.Errorf("didOpen of %q was accepted silently", uri)
		}
		id++
		if response := c.call(id, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}}); response["error"] == nil {
			t.Errorf("documentSymbol of %q answered %v", uri, response["result"])
		}
	}
	app.lspDocumentsMu.Lock()
	owned := len(app.lspDocuments)
	app.lspDocumentsMu.Unlock()
	if owned != 0 {
		t.Fatalf("%d escaping documents were registered", owned)
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "secret.go")); string(data) != "package secret\n" {
		t.Fatalf("outside file changed: %q", data)
	}
	// A relative symlink that stays inside the workspace is an ordinary document.
	if err := os.Symlink("main.go", filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if response := c.call(1, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": base + "/alias.go"}}); response["error"] != nil {
		t.Fatalf("inside symlink rejected: %v", response)
	}
}

func TestReviewLSPCancelledRequestsReleaseTheirSlots(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	c := reviewLSPDial(t, app, root, nil)
	// Requests that finish free their slot; more than the 64-request window in sequence is fine.
	for i := range 100 {
		if response := c.call(float64(i), "wingman/documentCapabilities", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}}); response["error"] != nil {
			t.Fatalf("request %d: %v", i, response)
		}
	}
	// String and numeric ids are distinct.
	for _, id := range []any{"7", 7} {
		if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "wingman/documentCapabilities", "params": map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}}}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		message, ok := c.next(5*time.Second, func(m map[string]any) bool { return m["id"] != nil })
		if !ok || message["error"] != nil {
			t.Fatalf("response = %v", message)
		}
		seen[fmt.Sprintf("%T", message["id"])] = true
	}
	if !seen["string"] || !seen["float64"] {
		t.Fatalf("ids = %v", seen)
	}
}
