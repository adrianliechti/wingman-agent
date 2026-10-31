package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/coder/websocket/wsjson"
	godap "github.com/google/go-dap"
)

type browserTestTools string

func (p browserTestTools) Resolve(command string) string {
	if command == "fixture" {
		return string(p)
	}
	return ""
}

// Exercise browser DAP -> host manager -> Content-Length adapter -> browser,
// including state-scoped references and process survival after socket loss.
func TestDAPWebSocketLaunchInspectReconnect(t *testing.T) {
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc main() {}\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	app.workspace.DAP.Close()
	app.workspace.DAP = dap.NewManager(root, browserTestTools(executable), dap.AdapterDescriptor{Name: "fixture", Language: "Test", Command: "fixture", Args: []string{"-test.run=^TestDAPBrowserAdapterProcess$", "--", "wingman-dap-fixture"}, Transport: dap.TransportStdio, SourceExtensions: []string{".go"}})
	conn, ctx := dialEditorTest(t, app, "/dap")
	seq := float64(0)
	call := func(command string, args any) map[string]any {
		seq++
		response := editorTestReply(t, conn, ctx, map[string]any{"seq": seq, "type": "request", "command": command, "arguments": args})
		if response["success"] != true {
			t.Fatalf("%s failed: %v", command, response)
		}
		return response["body"].(map[string]any)
	}
	call("initialize", map[string]any{"adapterID": "wingman"})
	plan := debugLaunchPlan{Adapter: "fixture", Request: "launch", ProjectDir: ".", IO: "output", Configuration: map[string]any{"program": filepath.Join(root, "main.go")}}
	planID := app.workspace.DAP.RememberPlan(plan)
	seq++
	rejected := editorTestReply(t, conn, ctx, map[string]any{"seq": seq, "type": "request", "command": "launch", "arguments": map[string]any{"_wingmanPlanId": "unknown", "_wingmanPlan": plan}})
	if rejected["success"] != false {
		t.Fatal("launch accepted a plan the host never reviewed", rejected)
	}
	seq++
	launchSeq := seq
	edited := plan
	edited.PreLaunch = &dap.ProcessLaunch{Title: "injected", Command: "/bin/sh", Args: []string{"-c", "touch injected"}}
	if err := wsjson.Write(ctx, conn, map[string]any{"seq": seq, "type": "request", "command": "launch", "arguments": map[string]any{"_wingmanPlanId": planID, "_wingmanPlan": edited}}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "initialized" {
			break
		}
	}
	seq++
	configSeq := seq
	if err := wsjson.Write(ctx, conn, map[string]any{"seq": seq, "type": "request", "command": "configurationDone"}); err != nil {
		t.Fatal(err)
	}
	responses := map[float64]bool{}
	for !responses[launchSeq] || !responses[configSeq] {
		var message map[string]any
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] == "response" {
			if message["success"] != true {
				t.Fatal(message)
			}
			responses[message["request_seq"].(float64)] = true
		}
	}
	status := call("wingman/status", nil)
	id := status["session"].(map[string]any)["session_id"]
	if status["session"].(map[string]any)["state"] != "stopped" {
		t.Fatal(status)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); err == nil {
		t.Fatal("launch ran a client-supplied prelaunch command")
	}
	frames := call("stackTrace", map[string]any{"threadId": 1})["stackFrames"].([]any)
	frame := frames[0].(map[string]any)["id"]
	scopes := call("scopes", map[string]any{"frameId": frame})["scopes"].([]any)
	reference := scopes[0].(map[string]any)["variablesReference"]
	variables := call("variables", map[string]any{"variablesReference": reference})["variables"].([]any)
	if variables[0].(map[string]any)["value"] != "42" {
		t.Fatal(variables)
	}
	if call("evaluate", map[string]any{"frameId": frame, "expression": "answer + 1"})["result"] != "43" {
		t.Fatal("evaluation was not forwarded")
	}
	call("continue", map[string]any{"threadId": 1})
	seq++
	rejected = editorTestReply(t, conn, ctx, map[string]any{"seq": seq, "type": "request", "command": "variables", "arguments": map[string]any{"variablesReference": reference}})
	if rejected["success"] != false {
		t.Fatal("reference survived resume", rejected)
	}
	_ = conn.CloseNow()
	conn, ctx = dialEditorTest(t, app, "/dap")
	call("initialize", map[string]any{"adapterID": "wingman"})
	status = call("wingman/status", nil)
	if status["session"].(map[string]any)["session_id"] != id || status["output"] != "fixture ready\n" || status["_wingmanOutputOffset"] != float64(len("fixture ready\n")) {
		t.Fatal("reconnect did not retain the process and output", status)
	}
	seq++
	rejected = editorTestReply(t, conn, ctx, map[string]any{"seq": seq, "type": "request", "command": "scopes", "arguments": map[string]any{"frameId": frame}})
	if rejected["success"] != false || rejected["message"] != "stateChanged" {
		t.Fatal("reference survived reconnect", rejected)
	}
	// DAP terminates a launched debuggee when disconnect omits terminateDebuggee.
	seq++
	disconnectSeq := seq
	if err := wsjson.Write(ctx, conn, map[string]any{"seq": seq, "type": "request", "command": "disconnect"}); err != nil {
		t.Fatal(err)
	}
	events := map[string]int{}
	for {
		var message map[string]any
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] == "event" {
			events[message["event"].(string)]++
		}
		if message["type"] == "response" && message["request_seq"] == float64(disconnectSeq) {
			if message["success"] != true {
				t.Fatal(message)
			}
			break
		}
	}
	if call("wingman/status", nil)["session"] != nil {
		t.Fatal("disconnect did not stop the process")
	}
	if events["output"] != 0 || events["exited"] > 1 || events["terminated"] > 1 {
		t.Fatalf("reconnected client received replayed output or duplicate lifecycle events: %v", events)
	}
}

// Rapid gutter edits must reach the host in order.
func TestDAPWebSocketOrdersBreakpointRequests(t *testing.T) {
	root := t.TempDir()
	writeDebugTestFile(t, root, "main.go", "package main\nfunc main() {}\n")
	app := newDebugTestServer(t, root)
	app.scope = WorkspaceScope{"workspace", "instance"}
	conn, ctx := dialEditorTest(t, app, "/dap")
	if response := editorTestReply(t, conn, ctx, map[string]any{"seq": float64(1), "type": "request", "command": "initialize", "arguments": map[string]any{"adapterID": "wingman"}}); response["success"] != true {
		t.Fatal(response)
	}
	source := map[string]any{"path": filepath.Join(root, "main.go")}
	for line := 1; line <= 60; line++ {
		request := map[string]any{"seq": float64(line + 1), "type": "request", "command": "setBreakpoints", "arguments": map[string]any{"source": source, "breakpoints": []any{map[string]any{"line": line}}}}
		if err := wsjson.Write(ctx, conn, request); err != nil {
			t.Fatal(err)
		}
	}
	var last map[string]any
	for last == nil {
		var message map[string]any
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] == "response" && message["request_seq"] == float64(61) {
			last = message
		}
	}
	status := editorTestReply(t, conn, ctx, map[string]any{"seq": float64(62), "type": "request", "command": "wingman/status", "arguments": map[string]any{"source": source}})
	points := status["body"].(map[string]any)["breakpoints"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["line"] != float64(60) {
		t.Fatalf("the last setBreakpoints must win: %v", points)
	}
	resolved := last["body"].(map[string]any)["breakpoints"].([]any)[0].(map[string]any)
	if resolved["reason"] != "pending" || resolved["verified"] != false || resolved["id"] == nil {
		t.Fatalf("breakpoints without a session must be pending with an id: %v", resolved)
	}
}

func TestDAPBrowserAdapterProcess(t *testing.T) {
	if !slices.Contains(os.Args, "wingman-dap-fixture") {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	seq := 0
	send := func(message map[string]any) {
		seq++
		message["seq"] = seq
		data, _ := json.Marshal(message)
		if godap.WriteBaseMessage(os.Stdout, data) != nil {
			os.Exit(2)
		}
	}
	event := func(name string, body any) { send(map[string]any{"type": "event", "event": name, "body": body}) }
	var launch map[string]any
	for {
		data, err := godap.ReadBaseMessage(reader)
		if err != nil {
			os.Exit(0)
		}
		var request map[string]any
		if json.Unmarshal(data, &request) != nil {
			os.Exit(2)
		}
		command := request["command"].(string)
		body := map[string]any{}
		switch command {
		case "initialize":
			body["supportsConfigurationDoneRequest"] = true
		case "launch":
			launch = request
			event("initialized", nil)
			continue
		case "threads":
			body["threads"] = []any{map[string]any{"id": 1, "name": "main"}}
		case "stackTrace":
			body["stackFrames"] = []any{map[string]any{"id": 10, "name": "main", "line": 2, "column": 1}}
			body["totalFrames"] = 1
		case "scopes":
			body["scopes"] = []any{map[string]any{"name": "Locals", "variablesReference": 20, "expensive": false}}
		case "variables":
			body["variables"] = []any{map[string]any{"name": "answer", "value": "42", "type": "int", "variablesReference": 0}}
		case "evaluate":
			body["result"] = "43"
			body["variablesReference"] = 0
		case "continue":
			body["allThreadsContinued"] = true
		}
		send(map[string]any{"type": "response", "request_seq": request["seq"], "command": command, "success": true, "body": body})
		switch command {
		case "configurationDone":
			event("output", map[string]any{"category": "console", "output": "fixture ready\n"})
			event("stopped", map[string]any{"reason": "breakpoint", "threadId": 1, "allThreadsStopped": true})
			send(map[string]any{"type": "response", "request_seq": launch["seq"], "command": "launch", "success": true, "body": map[string]any{}})
		case "continue":
			event("stopped", map[string]any{"reason": "step", "threadId": 1, "allThreadsStopped": true})
		case "disconnect":
			os.Exit(0)
		}
	}
}
