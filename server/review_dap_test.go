package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	godap "github.com/google/go-dap"
)

type reviewDAPTools string

func (path reviewDAPTools) Resolve(command string) string {
	if command == "review-fixture" {
		return string(path)
	}
	return ""
}

// reviewDAPServer installs the fixture adapter below. Options: "children"
// serves TCP and starts two workers over startDebugging; "slow" delays
// initialize.
func reviewDAPServer(t *testing.T, options ...string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc main() {}\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := dap.AdapterDescriptor{
		Name: "review", Language: "Test", Command: "review-fixture", Transport: dap.TransportStdio, SourceExtensions: []string{".go"},
		Args: append([]string{"-test.run=^TestReviewDAPFixtureAdapterProcess$", "--", "review-dap-fixture"}, options...),
	}
	if slices.Contains(options, "children") {
		descriptor.Transport, descriptor.ReadyPrefix = dap.TransportTCP, "Review DAP listening at "
	}
	app.workspace.DAP.Close()
	app.workspace.DAP = dap.NewManager(root, reviewDAPTools(executable), descriptor)
	return app, root
}

func reviewDAPRemember(app *Server, root, request string) string {
	return app.workspace.DAP.RememberPlan(debugLaunchPlan{Adapter: "review", Request: request, ProjectDir: ".", IO: "output", Configuration: map[string]any{"program": filepath.Join(root, "main.go")}})
}

// reviewDAPClient reads on its own goroutine so tests can wait with timeouts
// without closing the socket, and checks that gateway seq always increases.
type reviewDAPClient struct {
	t         *testing.T
	conn      *websocket.Conn
	seq       float64
	gateway   float64
	messages  chan map[string]any
	events    []map[string]any
	responses map[float64]map[string]any
}

func reviewDAPDial(t *testing.T, app *Server) *reviewDAPClient {
	t.Helper()
	conn, ctx := dialEditorTest(t, app, "/dap")
	c := &reviewDAPClient{t: t, conn: conn, messages: make(chan map[string]any, 256), responses: map[float64]map[string]any{}}
	go func() {
		defer close(c.messages)
		for {
			var message map[string]any
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				return
			}
			select {
			case c.messages <- message:
			case <-ctx.Done():
				return
			}
		}
	}()
	return c
}

func (c *reviewDAPClient) next(timeout time.Duration) (map[string]any, bool) {
	select {
	case message, ok := <-c.messages:
		if !ok {
			return nil, false
		}
		seq, _ := message["seq"].(float64)
		if seq <= c.gateway {
			c.t.Errorf("gateway seq %v follows %v: %v", seq, c.gateway, message)
		}
		c.gateway = seq
		switch message["type"] {
		case "event":
			c.events = append(c.events, message)
		case "response":
			c.responses[message["request_seq"].(float64)] = message
		}
		return message, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (c *reviewDAPClient) send(command string, arguments any) float64 {
	c.seq++
	if err := c.conn.Write(context.Background(), websocket.MessageText, reviewDAPJSON(map[string]any{"seq": c.seq, "type": "request", "command": command, "arguments": arguments})); err != nil {
		c.t.Errorf("send %s: %v", command, err)
	}
	return c.seq
}

func reviewDAPJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// reply returns the response to seq; DAP responses may arrive in any order.
func (c *reviewDAPClient) reply(seq float64) map[string]any {
	for {
		if response, ok := c.responses[seq]; ok {
			delete(c.responses, seq)
			return response
		}
		if _, ok := c.next(20 * time.Second); !ok {
			c.t.Errorf("no response to request %v", seq)
			return map[string]any{}
		}
	}
}

func (c *reviewDAPClient) call(command string, arguments any) map[string]any {
	response := c.reply(c.send(command, arguments))
	if response["command"] != nil && response["command"] != command {
		c.t.Errorf("response to %s echoes command %v", command, response["command"])
	}
	return response
}

func (c *reviewDAPClient) must(command string, arguments any) map[string]any {
	c.t.Helper()
	response := c.call(command, arguments)
	if response["success"] != true {
		c.t.Fatalf("%s failed: %v", command, response)
	}
	body, _ := response["body"].(map[string]any)
	return body
}

// waitEvent consumes the first buffered or future event that matches.
func (c *reviewDAPClient) waitEvent(name string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		for index, event := range c.events {
			body, _ := event["body"].(map[string]any)
			if event["event"] == name && (match == nil || match(body)) {
				c.events = slices.Delete(c.events, index, index+1)
				return body
			}
		}
		if _, ok := c.next(time.Until(deadline)); !ok {
			c.t.Fatalf("no %s event; saw %v", name, c.events)
		}
	}
}

func (c *reviewDAPClient) drain(duration time.Duration) {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if _, ok := c.next(time.Until(deadline)); !ok {
			return
		}
	}
}

func (c *reviewDAPClient) launch(command, planID string) {
	c.t.Helper()
	seq := c.send(command, map[string]any{"_wingmanPlanId": planID})
	c.waitEvent("initialized", nil)
	c.must("configurationDone", nil)
	if response := c.reply(seq); response["success"] != true {
		c.t.Fatalf("%s failed: %v", command, response)
	}
}

func reviewDAPSession(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	session, _ := body["session"].(map[string]any)
	if session == nil {
		t.Fatalf("no session in %v", body)
	}
	return session
}

func reviewDAPError(response map[string]any) string {
	body, _ := response["body"].(map[string]any)
	detail, _ := body["error"].(map[string]any)
	format, _ := detail["format"].(string)
	return format
}

// Bug: when one startDebugging child ends while another keeps running, the gateway sends exited and terminated for the whole session, which a DAP client treats as the end of debugging.
func TestReviewDAPChildEndDoesNotTerminateBrowserSession(t *testing.T) {
	app, root := reviewDAPServer(t, "children")
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	c.launch("launch", reviewDAPRemember(app, root, "launch"))
	deadline := time.Now().Add(15 * time.Second)
	for len(c.must("threads", nil)["threads"].([]any)) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("both workers never attached")
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.drain(200 * time.Millisecond)
	c.events = nil
	// The focused worker B evaluates "end" and exits; worker A keeps running.
	c.must("evaluate", map[string]any{"expression": "end"})
	c.drain(3 * time.Second)
	for _, event := range c.events {
		if event["event"] == "terminated" || event["event"] == "exited" {
			t.Errorf("gateway sent %s while worker A still runs: %v", event["event"], event["body"])
		}
	}
	if state := reviewDAPSession(t, c.must("wingman/status", nil))["state"]; state != "running" {
		t.Fatalf("session state = %v after worker B ended", state)
	}
	c.must("disconnect", nil)
}

// Bug: closing the socket after configurationDone, for example a page reload during a slow adapter start, cancels the launch and kills the adapter, although sockets otherwise never own debug processes.
func TestReviewDAPSocketCloseAfterConfigurationKeepsLaunch(t *testing.T) {
	app, root := reviewDAPServer(t, "slow")
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	c.send("launch", map[string]any{"_wingmanPlanId": reviewDAPRemember(app, root, "launch")})
	c.waitEvent("initialized", nil)
	c.must("configurationDone", nil)
	_ = c.conn.CloseNow()
	d := reviewDAPDial(t, app)
	d.must("initialize", map[string]any{"adapterID": "wingman"})
	// The adapter answers initialize after 1.5s; depending on timing the launch is
	// abandoned before Start or cancelled inside it.
	deadline := time.Now().Add(6 * time.Second)
	var session map[string]any
	for session == nil {
		session, _ = d.must("wingman/status", nil)["session"].(map[string]any)
		if time.Now().After(deadline) {
			t.Fatal("configured launch was abandoned when its socket closed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if session["state"] != "stopped" || session["error"] != nil {
		t.Fatalf("configured launch did not survive the socket: %v", session)
	}
	d.must("disconnect", nil)
}

// An explicit cancel still stops a launch that outlives its socket.
func TestReviewDAPCancelAfterConfigurationStopsLaunch(t *testing.T) {
	app, root := reviewDAPServer(t, "slow")
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	launch := c.send("launch", map[string]any{"_wingmanPlanId": reviewDAPRemember(app, root, "launch")})
	c.waitEvent("initialized", nil)
	c.must("configurationDone", nil)
	c.must("cancel", map[string]any{"requestId": launch})
	if response := c.reply(launch); response["success"] != false || response["message"] != "cancelled" {
		t.Fatalf("cancelled launch = %v", response)
	}
	if session, _ := c.must("wingman/status", nil)["session"].(map[string]any); session != nil && session["state"] != "terminated" {
		t.Fatalf("cancelled launch left a session: %v", session)
	}
	release, err := app.workspace.DAP.BeginPreparation()
	if err != nil {
		t.Fatalf("cancelled launch left the debugger busy: %v", err)
	}
	release()
}

// Bug: setBreakpoints requires the source to exist, so breakpoints of a deleted or renamed file can never be cleared and are sent to every later session.
func TestReviewDAPClearBreakpointsOfDeletedFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "gone.go")
	writeDebugTestFile(t, root, "gone.go", "package main\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	c.must("setBreakpoints", map[string]any{"source": map[string]any{"path": source}, "breakpoints": []any{map[string]any{"line": 1}}})
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	response := c.call("setBreakpoints", map[string]any{"source": map[string]any{"path": source}, "breakpoints": []any{}})
	if response["success"] != true || len(app.workspace.DAP.Breakpoints(source)) != 0 {
		t.Fatalf("clearing breakpoints of a deleted file: %v; still stored: %v", response, app.workspace.DAP.Breakpoints(source))
	}
}

func TestReviewDAPReconnectOutputContinuesFromStatusOffset(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	c.launch("launch", reviewDAPRemember(app, root, "launch"))
	ready := c.waitEvent("output", func(body map[string]any) bool { return body["output"] == "fixture ready\n" })
	if ready["_wingmanOffset"] != float64(0) || ready["category"] != "stdout" || ready["_wingmanSessionId"] == nil {
		t.Fatalf("first output = %v", ready)
	}
	c.must("evaluate", map[string]any{"expression": "print:one ✓\n"})
	one := c.waitEvent("output", func(body map[string]any) bool { return strings.Contains(body["output"].(string), "one") })
	if one["_wingmanOffset"] != float64(len("fixture ready\n")) {
		t.Fatalf("second output = %v", one)
	}
	_ = c.conn.CloseNow()

	d := reviewDAPDial(t, app)
	d.must("initialize", map[string]any{"adapterID": "wingman"})
	status := d.must("wingman/status", nil)
	end := float64(len("fixture ready\n") + len("one ✓\n") - 2) // ✓ is one UTF-16 unit but three bytes
	if status["output"] != "fixture ready\none ✓\n" || status["_wingmanOutputOffset"] != end {
		t.Fatalf("restored output = %q at %v", status["output"], status["_wingmanOutputOffset"])
	}
	d.must("evaluate", map[string]any{"expression": "print:two\n"})
	two := d.waitEvent("output", func(body map[string]any) bool { return strings.Contains(body["output"].(string), "two") })
	if two["_wingmanOffset"] != end || two["output"] != "two\n" {
		t.Fatalf("output after reconnect = %v, want it to continue at %v", two, end)
	}
	for _, event := range d.events {
		if body, _ := event["body"].(map[string]any); event["event"] == "output" && body["output"] != "two\n" {
			t.Fatalf("reconnected client received replayed output %v", body)
		}
	}
	d.must("disconnect", nil)
}

func TestReviewDAPReferencesExpireAcrossSessions(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	planID := reviewDAPRemember(app, root, "launch")
	c.launch("launch", planID)
	first := reviewDAPSession(t, c.must("wingman/status", nil))
	frame := c.must("stackTrace", map[string]any{"threadId": 1})["stackFrames"].([]any)[0].(map[string]any)["id"]
	scope := c.must("scopes", map[string]any{"frameId": frame})["scopes"].([]any)[0].(map[string]any)["variablesReference"]
	if response := c.call("completions", map[string]any{"text": "a"}); response["message"] != "notSupported" {
		t.Fatalf("unsupported command = %v", response)
	}
	c.must("disconnect", nil)
	if c.must("wingman/status", nil)["session"] != nil {
		t.Fatal("disconnect left the session active")
	}
	// A reviewed plan can launch again, but nothing from the first session reaches the second.
	c.launch("launch", planID)
	second := reviewDAPSession(t, c.must("wingman/status", nil))
	if second["session_id"] == first["session_id"] {
		t.Fatal("a new launch reused the session id")
	}
	fresh := c.must("stackTrace", map[string]any{"threadId": 1})["stackFrames"].([]any)[0].(map[string]any)["id"]
	if fresh == frame {
		t.Fatal("a frame id was reused by the next session")
	}
	for name, arguments := range map[string]map[string]any{
		"scopes":    {"frameId": frame},
		"variables": {"variablesReference": scope},
		"evaluate":  {"frameId": frame, "expression": "answer"},
	} {
		if response := c.call(name, arguments); response["success"] != false || response["message"] != "stateChanged" {
			t.Fatalf("%s with a reference from the first session = %v", name, response)
		}
	}
	if response := c.call("stackTrace", map[string]any{"threadId": 1, "_wingmanSessionId": first["session_id"]}); response["success"] != false {
		t.Fatalf("stackTrace for the ended session = %v", response)
	}
	stale := map[string]any{"threadId": 1, "_wingmanSessionId": second["session_id"], "_wingmanStateVersion": second["state_version"].(float64) + 1000}
	for _, name := range []string{"stackTrace", "continue", "next"} {
		if response := c.call(name, stale); response["message"] != "stateChanged" {
			t.Fatalf("%s with a stale state version = %v", name, response)
		}
	}
	c.must("disconnect", nil)
}

func TestReviewDAPPendingBreakpointsVerifyOnLaunch(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	source := map[string]any{"path": filepath.Join(root, "main.go")}
	pending := c.must("setBreakpoints", map[string]any{"source": source, "breakpoints": []any{map[string]any{"line": 2}}})["breakpoints"].([]any)[0].(map[string]any)
	id := pending["id"]
	if pending["reason"] != "pending" || pending["verified"] != false || id == nil {
		t.Fatalf("pending breakpoint = %v", pending)
	}
	c.launch("launch", reviewDAPRemember(app, root, "launch"))
	stopped := c.waitEvent("stopped", nil)
	if hits, _ := stopped["hitBreakpointIds"].([]any); stopped["reason"] != "breakpoint" || len(hits) != 1 || hits[0] != id {
		t.Fatalf("stopped = %v, want the gateway id %v", stopped, id)
	}
	c.waitEvent("breakpoint", func(body map[string]any) bool {
		breakpoint, _ := body["breakpoint"].(map[string]any)
		return body["reason"] == "changed" && breakpoint["id"] == id && breakpoint["verified"] == true
	})
	status := c.must("wingman/status", map[string]any{"source": source})
	resolved := status["resolved"].([]any)
	if len(resolved) != 1 || resolved[0].(map[string]any)["id"] != id || resolved[0].(map[string]any)["verified"] != true {
		t.Fatalf("resolved = %v", resolved)
	}
	c.must("disconnect", nil)
}

// reviewDAPDisconnect launches with request, disconnects with arguments and
// returns the terminateDebuggee value the adapter received ("<nil>" if absent).
func reviewDAPDisconnect(t *testing.T, app *Server, root string, c *reviewDAPClient, request string, arguments map[string]any) string {
	t.Helper()
	marker := filepath.Join(root, "review-dap-disconnect")
	_ = os.Remove(marker)
	c.launch(request, reviewDAPRemember(app, root, request))
	c.must("disconnect", arguments)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReviewDAPDisconnectDefaultsFollowRequest(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	if got := reviewDAPDisconnect(t, app, root, c, "launch", nil); got != "true" {
		t.Fatalf("launch disconnect sent terminateDebuggee %s", got)
	}
	if got := reviewDAPDisconnect(t, app, root, c, "attach", nil); got == "true" {
		t.Fatal("attach disconnect terminated the debuggee")
	}
	if got := reviewDAPDisconnect(t, app, root, c, "attach", map[string]any{"terminateDebuggee": true}); got != "true" {
		t.Fatalf("attach disconnect with terminateDebuggee sent %s", got)
	}
}

// Bug: go-dap omits a false terminateDebuggee, so a client's explicit request to keep a launched debuggee running reaches the adapter unspecified and the adapter's launch default terminates it.
func TestReviewDAPDisconnectKeepsLaunchedDebuggeeWhenAsked(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	if got := reviewDAPDisconnect(t, app, root, c, "launch", map[string]any{"terminateDebuggee": false}); got != "false" {
		t.Fatalf("adapter received terminateDebuggee %s, want an explicit false", got)
	}
}

func TestReviewDAPLaunchHandshakeEdgeCases(t *testing.T) {
	root := t.TempDir()
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	if response := c.call("configurationDone", nil); response["success"] != false {
		t.Fatalf("configurationDone without a launch = %v", response)
	}
	if response := c.call("launch", map[string]any{"program": "/bin/sh"}); response["success"] != false || !strings.Contains(reviewDAPError(response), "_wingmanPlanId") {
		t.Fatalf("launch without a plan = %v", response)
	}
	planID := app.workspace.DAP.RememberPlan(debugLaunchPlan{Adapter: "missing", Request: "launch", ProjectDir: ".", IO: "output"})
	if response := c.call("attach", map[string]any{"_wingmanPlanId": planID}); response["success"] != false {
		t.Fatalf("attach with a launch plan = %v", response)
	}
	first := c.send("launch", map[string]any{"_wingmanPlanId": planID})
	c.waitEvent("initialized", nil)
	if response := c.call("launch", map[string]any{"_wingmanPlanId": planID}); response["success"] != false || !strings.Contains(reviewDAPError(response), "in progress") {
		t.Fatalf("second launch = %v", response)
	}
	if response := c.call("cancel", map[string]any{"requestId": first}); response["success"] != true {
		t.Fatalf("cancel = %v", response)
	}
	if response := c.reply(first); response["success"] != false || response["message"] != "cancelled" {
		t.Fatalf("cancelled launch = %v", response)
	}
	if response := c.call("configurationDone", nil); response["success"] != false {
		t.Fatalf("configurationDone after the launch was cancelled = %v", response)
	}
	// A cancelled handshake must not block the next launch.
	second := c.send("launch", map[string]any{"_wingmanPlanId": planID})
	c.waitEvent("initialized", nil)
	c.must("configurationDone", nil)
	if response := c.reply(second); response["success"] != false || !strings.Contains(reviewDAPError(response), "missing") {
		t.Fatalf("launch with an unavailable adapter = %v", response)
	}
	initialized := 0
	for _, event := range c.events {
		if event["event"] == "initialized" {
			initialized++
		}
	}
	if initialized != 0 {
		t.Fatalf("rejected launches sent %d initialized events", initialized)
	}
	if _, err := app.workspace.DAP.BeginPreparation(); err != nil {
		t.Fatalf("failed launches left the debugger busy: %v", err)
	}
}

func TestReviewDAPEnvelopeAndErrorCodes(t *testing.T) {
	root := t.TempDir()
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	c := reviewDAPDial(t, app)
	response := c.call("threads", nil)
	if response["success"] != false || response["message"] != "failed" || reviewDAPError(response) != "initialize is required" {
		t.Fatalf("request before initialize = %v", response)
	}
	if body, _ := response["body"].(map[string]any); body["error"].(map[string]any)["showUser"] != true {
		t.Fatalf("error body = %v", body)
	}
	for _, arguments := range []map[string]any{{"linesStartAt1": false}, {"columnsStartAt1": false}, {"pathFormat": "uri"}} {
		if response := c.call("initialize", arguments); response["success"] != false {
			t.Fatalf("initialize %v = %v", arguments, response)
		}
	}
	body := c.must("initialize", map[string]any{"adapterID": "wingman", "linesStartAt1": true, "pathFormat": "path"})
	for _, capability := range []string{"supportsConfigurationDoneRequest", "supportsCancelRequest", "supportTerminateDebuggee", "supportsTerminateRequest"} {
		if body[capability] != true {
			t.Fatalf("initialize does not advertise %s: %v", capability, body)
		}
	}
	if response := c.call("initialize", nil); response["success"] != false {
		t.Fatalf("second initialize = %v", response)
	}
	if response := c.call("stackTrace", map[string]any{"threadId": "one"}); response["success"] != false || reviewDAPError(response) != "invalid DAP arguments" {
		t.Fatalf("malformed arguments = %v", response)
	}
	if response := c.call("threads", nil); response["success"] != false || !strings.Contains(reviewDAPError(response), "no debug session") {
		t.Fatalf("threads without a session = %v", response)
	}
	if response := c.call("cancel", map[string]any{"requestId": 999}); response["success"] != true {
		t.Fatalf("cancel of an unknown request = %v", response)
	}
	for _, path := range []string{"../outside.go", "/etc/hosts", root, ""} {
		if response := c.call("setBreakpoints", map[string]any{"source": map[string]any{"path": path}, "breakpoints": []any{map[string]any{"line": 1}}}); response["success"] != false {
			t.Fatalf("setBreakpoints for %q = %v", path, response)
		}
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, reviewDAPJSON(map[string]any{"seq": 99, "type": "event", "event": "stopped"})); err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := c.next(5 * time.Second); !ok {
			break
		}
	}
	select {
	case _, open := <-c.messages:
		if open {
			t.Fatal("gateway kept the socket after a non-request message")
		}
	default:
		t.Fatal("gateway kept the socket after a non-request message")
	}
}

func TestReviewDAPReviewedPlanRejectsTampering(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc main() {}\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	app := newDebugTestServer(t, root)
	manager := app.workspace.DAP
	stored := debugLaunchPlan{
		Action: "debug", Title: "Main", Adapter: "delve", ProjectDir: ".", Request: "launch", IO: "output",
		Configuration:       map[string]any{"program": "main.go", "args": []any{"one"}},
		Breakpoints:         []debugPlanBreakpoint{{FilePath: "main.go", Line: 2}},
		FunctionBreakpoints: []string{"main.main"},
		PreLaunch:           &dap.ProcessLaunch{Title: "build", Command: "/usr/bin/true", Args: []string{"build"}, WaitForExit: true},
	}
	id := manager.RememberPlan(cloneDebugLaunchPlan(stored))
	edits := &debugLaunchPlan{
		Action: "run", Title: "evil", Adapter: "other", ProjectDir: "..", Request: "attach", IO: "terminal",
		Configuration: map[string]any{"program": "main.go", "stopOnEntry": true},
		Breakpoints:   []debugPlanBreakpoint{}, FunctionBreakpoints: []string{"main.main"},
		PreLaunch: &dap.ProcessLaunch{Title: "evil", Command: "/bin/sh", Args: []string{"-c", "touch pwned"}},
	}
	plan, err := app.reviewedDebugPlan(manager, id, edits)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Adapter != "delve" || plan.ProjectDir != "." || plan.Request != "launch" || plan.Action != "debug" || plan.Title != "Main" || plan.PreLaunch.Command != "/usr/bin/true" {
		t.Fatalf("host-owned fields came from the client: %+v", plan)
	}
	if plan.IO != "terminal" || plan.Configuration["stopOnEntry"] != true || len(plan.Breakpoints) != 0 || !slices.Equal(plan.FunctionBreakpoints, []string{"main.main"}) {
		t.Fatalf("reviewable choices were not applied: %+v", plan)
	}
	plan.PreLaunch.Args[0] = "mutated"
	again, err := app.reviewedDebugPlan(manager, id, nil)
	if err != nil || again.PreLaunch.Args[0] != "build" || again.Configuration["stopOnEntry"] != nil || len(again.Breakpoints) != 1 {
		t.Fatalf("a launch mutated the reviewed plan: %+v, %v", again, err)
	}
	for name, edit := range map[string]*debugLaunchPlan{
		"new breakpoint":       {Breakpoints: []debugPlanBreakpoint{{FilePath: "main.go", Line: 1}}},
		"conditional":          {Breakpoints: []debugPlanBreakpoint{{FilePath: "main.go", Line: 2, Condition: "true"}}},
		"logpoint":             {Breakpoints: []debugPlanBreakpoint{{FilePath: "main.go", Line: 2, LogMessage: "{os.Exit(1)}"}}},
		"function breakpoint":  {FunctionBreakpoints: []string{"os.Exit"}},
		"outside breakpoint":   {Breakpoints: []debugPlanBreakpoint{{FilePath: "../main.go", Line: 2}}},
		"respelled breakpoint": {Breakpoints: []debugPlanBreakpoint{{FilePath: "./main.go", Line: 2}}},
	} {
		if _, err := app.reviewedDebugPlan(manager, id, edit); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := app.reviewedDebugPlan(manager, "unknown", nil); err == nil {
		t.Fatal("an unknown plan id was accepted")
	}
	adapters := []dap.AdapterInfo{{Name: "delve", Language: "Go", Projects: []string{root}, ConfigurationPaths: []dap.ConfigurationPath{{Key: "program"}, {Key: "cwd", Directory: true}}}}
	for name, mutate := range map[string]func(*debugLaunchPlan){
		"program outside":    func(plan *debugLaunchPlan) { plan.Configuration["program"] = "../outside.go" },
		"absolute program":   func(plan *debugLaunchPlan) { plan.Configuration["program"] = "/etc/hosts" },
		"variable program":   func(plan *debugLaunchPlan) { plan.Configuration["program"] = "${workspaceFolder}/main.go" },
		"symlinked cwd":      func(plan *debugLaunchPlan) { plan.Configuration["cwd"] = "escape" },
		"unknown io":         func(plan *debugLaunchPlan) { plan.IO = "pipe" },
		"terminal io":        func(plan *debugLaunchPlan) { plan.IO = "terminal" },
		"relative prelaunch": func(plan *debugLaunchPlan) { plan.PreLaunch.Command = "sh" },
		"remote ready url":   func(plan *debugLaunchPlan) { plan.PreLaunch.ReadyURL = "http://example.com/" },
		"file ready url":     func(plan *debugLaunchPlan) { plan.PreLaunch.ReadyURL = "file:///etc/hosts" },
		"project outside":    func(plan *debugLaunchPlan) { plan.ProjectDir = "escape" },
		"breakpoint line":    func(plan *debugLaunchPlan) { plan.Breakpoints[0].Line = 0 },
		"breakpoint outside": func(plan *debugLaunchPlan) { plan.Breakpoints[0].FilePath = "escape/x.go" },
		"unknown adapter":    func(plan *debugLaunchPlan) { plan.Adapter = "lldb" },
		"request":            func(plan *debugLaunchPlan) { plan.Request = "restart" },
	} {
		candidate, err := app.reviewedDebugPlan(manager, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&candidate)
		if err := app.validateDebugPlan(&candidate, adapters); err == nil {
			t.Errorf("%s passed validation", name)
		}
	}
	valid, _ := app.reviewedDebugPlan(manager, id, nil)
	valid.PreLaunch.ReadyURL = "http://127.0.0.1:5173/"
	if err := app.validateDebugPlan(&valid, adapters); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
}

// Several browsers share one session: breakpoint edits, status reads,
// stepping and output must stay ordered and race free.
func TestReviewDAPConcurrentClientsStress(t *testing.T) {
	app, root := reviewDAPServer(t)
	launcher := reviewDAPDial(t, app)
	launcher.must("initialize", map[string]any{"adapterID": "wingman"})
	launcher.launch("launch", reviewDAPRemember(app, root, "launch"))
	source := map[string]any{"path": filepath.Join(root, "main.go")}
	var wait sync.WaitGroup
	editor, printer := reviewDAPDial(t, app), reviewDAPDial(t, app)
	editor.must("initialize", map[string]any{"adapterID": "wingman"})
	printer.must("initialize", map[string]any{"adapterID": "wingman"})
	printer.must("wingman/status", nil)
	_, start := app.workspace.DAP.ActiveSession().OutputSnapshot()
	wait.Go(func() {
		for index := range 60 {
			breakpoints := []any{map[string]any{"line": 1 + index%2}, map[string]any{"line": 2, "condition": fmt.Sprint(index)}}
			if response := editor.call("setBreakpoints", map[string]any{"source": source, "breakpoints": breakpoints}); response["success"] != true {
				t.Errorf("setBreakpoints = %v", response)
				return
			}
			editor.call("wingman/status", map[string]any{"source": source})
		}
	})
	wait.Go(func() {
		for index := range 60 {
			if response := printer.call("evaluate", map[string]any{"expression": fmt.Sprintf("print:%03d\n", index)}); response["success"] != true {
				t.Errorf("evaluate = %v", response)
				return
			}
			printer.call("threads", nil)
		}
	})
	wait.Go(func() {
		for index := range 40 {
			launcher.call("setExceptionBreakpoints", map[string]any{"filters": []any{}})
			if index%10 == 0 {
				launcher.call("continue", map[string]any{"threadId": 1})
			}
			launcher.call("stackTrace", map[string]any{"threadId": 1})
		}
	})
	wait.Wait()
	points := app.workspace.DAP.Breakpoints(filepath.Join(root, "main.go"))
	if len(points) != 2 || points[1].Condition != "59" {
		t.Fatalf("last breakpoint edit lost: %+v", points)
	}
	printer.drain(500 * time.Millisecond)
	offset, text := float64(start), ""
	for _, event := range printer.events {
		body, _ := event["body"].(map[string]any)
		if event["event"] != "output" {
			continue
		}
		if body["_wingmanOffset"] != offset {
			t.Fatalf("output event at %v, want %v: %v", body["_wingmanOffset"], offset, body)
		}
		text += body["output"].(string)
		offset += float64(len(body["output"].(string)))
	}
	for index := range 60 {
		if !strings.Contains(text, fmt.Sprintf("%03d\n", index)) {
			t.Fatalf("output %03d was not delivered once: %q", index, text)
		}
	}
	launcher.must("disconnect", nil)
}

func TestReviewDAPLifecycleEventsOncePerConnection(t *testing.T) {
	app, root := reviewDAPServer(t)
	c := reviewDAPDial(t, app)
	c.must("initialize", map[string]any{"adapterID": "wingman"})
	c.launch("launch", reviewDAPRemember(app, root, "launch"))
	if stopped := c.waitEvent("stopped", nil); stopped["reason"] != "entry" || stopped["threadId"] != float64(1) {
		t.Fatalf("entry stop = %v", stopped)
	}
	c.must("continue", map[string]any{"threadId": 1})
	c.waitEvent("stopped", func(body map[string]any) bool { return body["reason"] == "step" })
	c.drain(200 * time.Millisecond)
	c.events = nil
	c.must("evaluate", map[string]any{"expression": "end"})
	c.drain(500 * time.Millisecond)
	var lifecycle []string
	for _, event := range c.events {
		switch name := event["event"].(string); name {
		case "exited":
			lifecycle = append(lifecycle, fmt.Sprintf("exited %v", event["body"].(map[string]any)["exitCode"]))
		case "terminated", "stopped", "continued":
			lifecycle = append(lifecycle, name)
		}
	}
	if !slices.Equal(lifecycle, []string{"exited 3", "terminated"}) {
		t.Fatalf("lifecycle events after the adapter exited = %v", lifecycle)
	}
	status := reviewDAPSession(t, c.must("wingman/status", nil))
	if status["state"] != "terminated" || status["exit_code"] != float64(3) {
		t.Fatalf("status after exit = %v", status)
	}
	// A later connection restores through status, not replayed events.
	d := reviewDAPDial(t, app)
	d.must("initialize", map[string]any{"adapterID": "wingman"})
	d.drain(300 * time.Millisecond)
	for _, event := range d.events {
		if name := event["event"]; name != "capabilities" {
			t.Fatalf("new connection received %v", event)
		}
	}
	c.events = nil
	c.must("disconnect", nil)
	c.drain(300 * time.Millisecond)
	for _, event := range c.events {
		if name := event["event"]; name == "terminated" || name == "exited" {
			t.Fatalf("removing the ended session repeated %s", name)
		}
	}
	if c.must("wingman/status", nil)["session"] != nil {
		t.Fatal("disconnect kept the ended session")
	}
}

// Two browsers repeatedly start and abandon launch handshakes; every waiter
// must be answered and none may leave the connection or manager busy.
func TestReviewDAPConcurrentLaunchCancelStress(t *testing.T) {
	root := t.TempDir()
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	planID := app.workspace.DAP.RememberPlan(debugLaunchPlan{Adapter: "missing", Request: "launch", ProjectDir: ".", IO: "output"})
	var wait sync.WaitGroup
	for range 3 {
		c := reviewDAPDial(t, app)
		c.must("initialize", map[string]any{"adapterID": "wingman"})
		wait.Go(func() {
			for index := range 25 {
				launch := c.send("launch", map[string]any{"_wingmanPlanId": planID})
				for initialized := false; !initialized; {
					message, ok := c.next(10 * time.Second)
					if !ok {
						t.Errorf("no initialized event in round %d", index)
						return
					}
					initialized = message["event"] == "initialized"
				}
				if index%2 == 0 {
					c.call("cancel", map[string]any{"requestId": launch})
					if response := c.reply(launch); response["message"] != "cancelled" {
						t.Errorf("cancelled launch = %v", response)
						return
					}
					continue
				}
				c.call("configurationDone", nil)
				if response := c.reply(launch); response["success"] != false || !strings.Contains(reviewDAPError(response), "missing") {
					t.Errorf("launch = %v", response)
					return
				}
			}
		})
	}
	wait.Wait()
	release, err := app.workspace.DAP.BeginPreparation()
	if err != nil {
		t.Fatalf("debugger left busy: %v", err)
	}
	release()
}

// TestReviewDAPFixtureAdapterProcess is the adapter process of the tests above.
func TestReviewDAPFixtureAdapterProcess(t *testing.T) {
	index := slices.Index(os.Args, "review-dap-fixture")
	if index < 0 {
		return
	}
	options := os.Args[index+1:]
	if slices.Contains(options, "children") {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		fmt.Printf("Review DAP listening at %s\n", listener.Addr())
		for {
			conn, err := listener.Accept()
			if err != nil {
				os.Exit(0)
			}
			go func() {
				defer conn.Close()
				reviewDAPFixtureServe(conn, conn, options)
			}()
		}
	}
	reviewDAPFixtureServe(os.Stdin, os.Stdout, options)
	os.Exit(0)
}

func reviewDAPFixtureServe(input io.Reader, output io.Writer, options []string) {
	reader := bufio.NewReader(input)
	var mu sync.Mutex
	seq := 0
	send := func(message map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		message["seq"] = seq
		data, _ := json.Marshal(message)
		if godap.WriteBaseMessage(output, data) != nil {
			os.Exit(2)
		}
	}
	event := func(name string, body any) { send(map[string]any{"type": "event", "event": name, "body": body}) }
	respond := func(request map[string]any, body any) {
		send(map[string]any{"type": "response", "request_seq": request["seq"], "command": request["command"], "success": true, "body": body})
	}
	replies := make(chan map[string]any, 4)
	var launch map[string]any
	name := ""
	var hits []int
	for {
		data, err := godap.ReadBaseMessage(reader)
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			os.Exit(2)
		}
		if message["type"] == "response" {
			replies <- message
			continue
		}
		arguments, _ := message["arguments"].(map[string]any)
		worker := strings.HasPrefix(name, "worker")
		switch message["command"] {
		case "initialize":
			if slices.Contains(options, "slow") {
				time.Sleep(1500 * time.Millisecond)
			}
			respond(message, map[string]any{"supportsConfigurationDoneRequest": true})
		case "launch", "attach":
			launch = message
			name, _ = arguments["name"].(string)
			event("initialized", map[string]any{})
		case "setBreakpoints":
			verified := []any{}
			list, _ := arguments["breakpoints"].([]any)
			for _, item := range list {
				line := int(item.(map[string]any)["line"].(float64))
				verified = append(verified, map[string]any{"id": 1000 + line, "verified": true, "line": line})
				hits = append(hits, 1000+line)
			}
			respond(message, map[string]any{"breakpoints": verified})
		case "configurationDone":
			respond(message, map[string]any{})
			if worker || slices.Contains(options, "children") {
				respond(launch, map[string]any{})
				if !worker {
					go func() {
						for _, child := range []string{"worker A", "worker B"} {
							send(map[string]any{"type": "request", "command": "startDebugging", "arguments": map[string]any{"request": "launch", "configuration": map[string]any{"name": child}}})
							<-replies
						}
					}()
				}
				continue
			}
			event("output", map[string]any{"category": "stdout", "output": "fixture ready\n"})
			stop := map[string]any{"reason": "entry", "threadId": 1, "allThreadsStopped": true}
			if len(hits) > 0 {
				stop["reason"], stop["hitBreakpointIds"] = "breakpoint", hits[:1]
			}
			event("stopped", stop)
			respond(launch, map[string]any{})
		case "threads":
			respond(message, map[string]any{"threads": []any{map[string]any{"id": 1, "name": "main"}}})
		case "stackTrace":
			respond(message, map[string]any{"stackFrames": []any{map[string]any{"id": 10, "name": "main", "line": 2, "column": 1}}, "totalFrames": 1})
		case "scopes":
			respond(message, map[string]any{"scopes": []any{map[string]any{"name": "Locals", "variablesReference": 20, "expensive": false}}})
		case "variables":
			respond(message, map[string]any{"variables": []any{map[string]any{"name": "answer", "value": "42", "variablesReference": 0}}})
		case "evaluate":
			expression, _ := arguments["expression"].(string)
			if text, ok := strings.CutPrefix(expression, "print:"); ok {
				event("output", map[string]any{"category": "stdout", "output": text})
			}
			respond(message, map[string]any{"result": "43", "variablesReference": 0})
			if expression == "end" {
				event("exited", map[string]any{"exitCode": 3})
				event("terminated", map[string]any{})
			}
		case "continue":
			respond(message, map[string]any{"allThreadsContinued": true})
			event("stopped", map[string]any{"reason": "step", "threadId": 1, "allThreadsStopped": true})
		case "disconnect":
			if name == "worker B" {
				time.Sleep(1500 * time.Millisecond)
			}
			if !worker {
				_ = os.WriteFile("review-dap-disconnect", []byte(fmt.Sprint(arguments["terminateDebuggee"])), 0o644)
			}
			respond(message, map[string]any{})
			if !worker {
				os.Exit(0)
			}
			return
		default:
			respond(message, map[string]any{})
		}
	}
}
