package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/acp-go-sdk"
)

// scriptedAgent answers app-server requests with handle and records every
// request method in order.
func scriptedAgent(t *testing.T, handle func(s *contractAppServer, req rpcMessage)) (*Agent, func() []string) {
	t.Helper()
	appIO, backendIO := net.Pipe()
	t.Cleanup(func() {
		_ = appIO.Close()
		_ = backendIO.Close()
	})
	rpc := newRPCClient(appIO, appIO)
	agent := newAgent(newCodexClient(rpc), "default", "")
	rpc.start()
	server := &contractAppServer{conn: backendIO}
	var mu sync.Mutex
	var methods []string
	go func() {
		scanner := bufio.NewScanner(backendIO)
		for scanner.Scan() {
			var req rpcMessage
			if json.Unmarshal(scanner.Bytes(), &req) != nil || len(req.ID) == 0 {
				continue
			}
			mu.Lock()
			methods = append(methods, req.Method)
			mu.Unlock()
			handle(server, req)
		}
	}()
	return agent, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), methods...)
	}
}

func TestCancelRetriesInterruptUntilTurnIsInterruptible(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failures   int
		message    string
		wantCalls  int32
		interrupts bool
	}{
		{"registers after retries", 2, "no active turn to interrupt", 3, true},
		{"gives up after bounded retries", 100, "no active turn to interrupt", 6, false},
		{"other errors are final", 100, "thread not loaded: thread-1", 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
					switch req.Method {
					case "turn/start":
						s.respond(req, turnStartResponse{Turn: turn{ID: "t1", Status: "inProgress"}})
					case "turn/interrupt":
						if int(calls.Add(1)) <= tt.failures {
							s.respondError(req, -32600, tt.message)
							return
						}
						s.respond(req, map[string]any{})
						s.notify("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "t1", "status": "interrupted"}})
					}
				})
				agent.sessions["thread-1"] = newSession("thread-1", "default", "", nil)
				done := make(chan acp.PromptResponse, 1)
				go func() {
					resp, err := agent.Prompt(context.Background(), acp.PromptRequest{SessionId: "thread-1", Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
					if err != nil {
						t.Errorf("prompt: %v", err)
					}
					done <- resp
				}()
				synctest.Wait()
				_ = agent.Cancel(context.Background(), acp.CancelNotification{SessionId: "thread-1"})
				if resp := <-done; resp.StopReason != acp.StopReasonCancelled {
					t.Fatalf("stop = %q", resp.StopReason)
				}
				if got := calls.Load(); got != tt.wantCalls {
					t.Fatalf("turn/interrupt calls = %d, want %d", got, tt.wantCalls)
				}
			})
		})
	}
}

func TestUnmaterializedThreadCanBeResumedLoadedAndDeleted(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "live thread", false: "unknown thread"}[known], func(t *testing.T) {
			agent, methods := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
				switch req.Method {
				case "config/read":
					s.respond(req, map[string]any{"config": map[string]any{}})
				case "thread/resume":
					s.respondError(req, -32600, "no rollout found for thread id thread-1")
				case "thread/read":
					if !known {
						s.respondError(req, -32600, "thread not loaded: thread-1")
						return
					}
					s.respond(req, map[string]any{"thread": map[string]any{"id": "thread-1", "model": "gpt-live", "reasoningEffort": "high", "historyMode": "paginated"}})
				case "thread/archive":
					s.respondError(req, -32600, "no rollout found for thread id thread-1")
				default:
					s.respond(req, map[string]any{})
				}
			})
			ctx := context.Background()
			resumed, err := agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "thread-1", Cwd: "/work"})
			if !known {
				if err == nil || !strings.Contains(err.Error(), "no rollout found") {
					t.Fatalf("unknown thread resume = %v", err)
				}
				if _, err := agent.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "thread-1", Cwd: "/work", McpServers: []acp.McpServer{}}); err == nil {
					t.Fatal("unknown thread was loaded")
				}
			} else {
				if err != nil || resumed.ConfigOptions[0].Select.CurrentValue != "gpt-live" {
					t.Fatalf("resume = %#v, %v", resumed, err)
				}
				if s := agent.lookup("thread-1"); s == nil || s.effort != "high" {
					t.Fatalf("resumed session = %#v", s)
				}
				if _, err := agent.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "thread-1", Cwd: "/work", McpServers: []acp.McpServer{}}); err != nil {
					t.Fatalf("load: %v", err)
				}
				for _, method := range methods() {
					if method == "thread/turns/list" {
						t.Fatal("unmaterialized thread history was paginated")
					}
				}
			}
			for _, message := range []string{"no rollout found for thread id thread-1", "invalid thread id: not-a-uuid", "thread not loaded: x"} {
				agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
					if req.Method == "thread/archive" {
						s.respondError(req, -32600, message)
						return
					}
					s.respond(req, map[string]any{})
				})
				if _, err := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "not-a-uuid"}); err != nil {
					t.Fatalf("delete with %q: %v", message, err)
				}
			}
		})
	}
	agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
		if req.Method == "thread/archive" {
			s.respondError(req, -32603, "disk full")
			return
		}
		s.respond(req, map[string]any{})
	})
	if _, err := agent.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{SessionId: "thread-1"}); err == nil {
		t.Fatal("archive failure was hidden")
	}
}

func TestMCPStartupAwaitTimeout(t *testing.T) {
	servers := []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "docs", Command: "docs-mcp"}}, {Http: &acp.McpServerHttpInline{Name: "remote", Url: "https://example.com/mcp"}}}
	status := func(s *contractAppServer, name, state string) {
		s.notify("mcpServer/startupStatus/updated", map[string]any{"threadId": "thread-1", "name": name, "status": state, "error": nil})
	}
	for _, tt := range []struct {
		name     string
		meta     map[string]any
		settle   bool
		wantWait time.Duration
	}{
		{"waits for terminal states", map[string]any{"mcpStartupAwaitTimeoutMs": float64(5000)}, true, time.Second},
		{"stops at timeout", map[string]any{"mcpStartupAwaitTimeoutMs": float64(3000)}, false, 3 * time.Second},
		{"omitted", nil, false, 0},
		{"non-positive", map[string]any{"mcpStartupAwaitTimeoutMs": float64(-1)}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
					if req.Method != "thread/start" {
						s.respond(req, map[string]any{})
						return
					}
					s.respond(req, map[string]any{"thread": map[string]any{"id": "thread-1"}, "model": "m"})
					status(s, "remote", "failed")
					if tt.settle {
						time.Sleep(time.Second)
						status(s, "docs", "ready")
					}
				})
				// A state from before the request must not satisfy this wait.
				agent.codex.recordMCPStartup(json.RawMessage(`{"threadId":"thread-1","name":"docs","status":"ready"}`))
				start := time.Now()
				if _, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/work", McpServers: servers, Meta: tt.meta}); err != nil {
					t.Fatal(err)
				}
				if waited := time.Since(start); waited != tt.wantWait {
					t.Fatalf("session/new waited %v, want %v", waited, tt.wantWait)
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
			s.respond(req, map[string]any{"thread": map[string]any{"id": "thread-1"}, "model": "m"})
			_ = s.conn.Close()
		})
		_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/work", McpServers: servers, Meta: map[string]any{"mcpStartupAwaitTimeoutMs": float64(5000)}})
		if err == nil || !strings.Contains(err.Error(), "MCP server startup") {
			t.Fatalf("backend exit during MCP startup = %v", err)
		}
	})
}

func TestMCPStartupWaitsForRequestedThread(t *testing.T) {
	server := &acp.McpServerStdio{Name: "docs", Command: "docs-mcp"}
	servers := []acp.McpServer{{Stdio: server}}
	meta := map[string]any{"mcpStartupAwaitTimeoutMs": 5000}
	for _, operation := range []string{"new", "resume", "fork"} {
		for _, terminal := range []string{"ready", "failed", "cancelled"} {
			t.Run(operation+"/"+terminal, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					threadID, otherID := "thread-1", "other-thread"
					if operation == "fork" {
						threadID, otherID = "fork-1", "thread-1"
					}
					agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
						switch req.Method {
						case "thread/start", "thread/resume", "thread/fork":
							s.respond(req, map[string]any{"thread": map[string]any{"id": threadID}, "model": "m"})
							status := func(id any, state string) {
								s.notify("mcpServer/startupStatus/updated", map[string]any{"threadId": id, "name": "docs", "status": state})
							}
							status(threadID, "starting")
							status(otherID, terminal)
							status(nil, terminal)
							time.Sleep(time.Second)
							status(threadID, terminal)
						default:
							s.respond(req, map[string]any{})
						}
					})
					ctx := context.Background()
					start := time.Now()
					var err error
					switch operation {
					case "new":
						_, err = agent.NewSession(ctx, acp.NewSessionRequest{Cwd: "/work", McpServers: servers, Meta: meta})
					case "resume":
						_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "thread-1", Cwd: "/work", McpServers: servers, Meta: meta})
					case "fork":
						_, err = agent.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{
							SessionId: "thread-1", Cwd: "/work", McpServers: []acp.UnstableMcpServer{{Stdio: server}}, Meta: meta,
						})
					}
					if err != nil {
						t.Fatal(err)
					}
					if waited := time.Since(start); waited != time.Second {
						t.Fatalf("session/%s waited %v, want to wait 1s for its own thread's MCP server", operation, waited)
					}
				})
			})
		}
	}
}

func TestInitializeNegotiatesRecommendedConfigValues(t *testing.T) {
	for _, negotiate := range []bool{true, false} {
		agent := newContractAgent(t).(*Agent)
		caps := acp.ClientCapabilities{}
		if negotiate {
			caps.Meta = map[string]any{"jetbrains": map[string]any{"air": map[string]any{"version": 1, "capabilities": []string{"recommendedValue"}}}}
		}
		init, err := agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, ClientCapabilities: caps})
		if err != nil {
			t.Fatal(err)
		}
		if !clientSupportsAirCapability(acp.ClientCapabilities{Meta: init.Meta}, recommendedValueCapability) {
			t.Fatalf("initialize meta = %#v", init.Meta)
		}
		session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/contract", McpServers: []acp.McpServer{}})
		if err != nil {
			t.Fatal(err)
		}
		model := session.ConfigOptions[0].Select
		if negotiate != (model.Meta != nil) {
			t.Fatalf("negotiated=%v model meta=%#v", negotiate, model.Meta)
		}
	}
}

func TestCommandApprovalCarriesCodexToolName(t *testing.T) {
	session, client := faultBackend(t, func(conn net.Conn, request rpcMessage) {
		replyTurnStarted(conn, request)
		writeRPCMessage(conn, rpcMessage{Method: "item/started", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"id":"cmd","type":"commandExecution","command":"make","source":"unifiedExecStartup","status":"inProgress"}}`)})
		writeRPCMessage(conn, rpcMessage{ID: json.RawMessage(`100`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","itemId":"cmd","command":"make"}`)})
		var reply rpcMessage
		_ = json.NewDecoder(conn).Decode(&reply)
		writeRPCMessage(conn, rpcMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"id":"cmd","type":"commandExecution","command":"make","source":"unifiedExecStartup","status":"completed"}}`)})
		writeRPCMessage(conn, rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"new-turn","status":"completed"}}`)})
	})
	agent := newAgent(client, "default", "")
	agent.sessions[session.id] = session
	peer := newProtocolPeer(t, agent)
	peer.send(t, `"prompt"`, "session/prompt", `{"sessionId":"thread-1","prompt":[{"type":"text","text":"Build"}]}`)
	named := 0
	for {
		frame := peer.next(t)
		if strings.Contains(string(frame.Params), `"toolName":"exec_command"`) {
			named++
		}
		switch frame.Method {
		case "session/request_permission":
			peer.reply(t, frame.ID, `{"outcome":{"outcome":"selected","optionId":"allow-once"}}`)
		case "session/update":
		case "":
			if frame.Error != nil || named != 3 {
				t.Fatalf("prompt = %+v; frames naming exec_command = %d, want start, approval, completion", frame, named)
			}
			return
		default:
			t.Fatalf("unexpected frame %+v", frame)
		}
	}
}

func TestWriteStdinApprovalKeepsRunningCommand(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "answered", true: "resolved by backend"}[resolved], func(t *testing.T) {
			shown := make(chan struct{})
			decision := make(chan any, 1)
			session, client := faultBackend(t, func(conn net.Conn, request rpcMessage) {
				replyTurnStarted(conn, request)
				writeRPCMessage(conn, rpcMessage{Method: "item/started", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"id":"cmd","type":"commandExecution","command":"python3","source":"unifiedExecStartup","status":"inProgress"}}`)})
				writeRPCMessage(conn, rpcMessage{ID: json.RawMessage(`100`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"kind":"writeStdin","threadId":"thread-1","turnId":"new-turn","itemId":"cmd","approvalId":"stdin-1","command":"write_stdin --session-id 7 'print(1)'","cwd":"/work","availableDecisions":["accept","cancel"]}`)})
				if resolved {
					<-shown
					writeRPCMessage(conn, rpcMessage{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread-1","requestId":100}`)})
				}
				var reply rpcMessage
				_ = json.NewDecoder(conn).Decode(&reply)
				var response execApprovalResponse
				_ = json.Unmarshal(reply.Result, &response)
				decision <- response.Decision
				writeRPCMessage(conn, rpcMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"id":"cmd","type":"commandExecution","command":"python3","source":"unifiedExecStartup","status":"completed"}}`)})
				writeRPCMessage(conn, rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"new-turn","status":"completed"}}`)})
			})
			agent := newAgent(client, "default", "")
			agent.sessions[session.id] = session
			peer := newProtocolPeer(t, agent)
			peer.send(t, `"prompt"`, "session/prompt", `{"sessionId":"thread-1","prompt":[{"type":"text","text":"Run"}]}`)
			var permissionID json.RawMessage
			approvalFinished := false
			for {
				frame := peer.next(t)
				switch frame.Method {
				case "session/request_permission":
					var request acp.RequestPermissionRequest
					if err := json.Unmarshal(frame.Params, &request); err != nil {
						t.Fatal(err)
					}
					tc := request.ToolCall
					if tc.ToolCallId != "approval:stdin-1" || tc.Title == nil || *tc.Title != "Send input to terminal" || toolNameFromMeta(tc.Meta) != "write_stdin" {
						t.Fatalf("permission tool call = %s", frame.Params)
					}
					if len(request.Options) != 2 || request.Options[0].OptionId != optionAllowOnce || request.Options[1].OptionId != optionCancel {
						t.Fatalf("options = %#v", request.Options)
					}
					permissionID = frame.ID
					if resolved {
						close(shown)
					} else {
						peer.reply(t, frame.ID, `{"outcome":{"outcome":"selected","optionId":"cancel"}}`)
					}
				case "$/cancel_request":
					if !resolved || !strings.Contains(string(frame.Params), string(permissionID)) {
						t.Fatalf("unexpected dismissal %s", frame.Params)
					}
					peer.reply(t, permissionID, `{"outcome":{"outcome":"selected","optionId":"allow-once"}}`)
				case "session/update":
					var n acp.SessionNotification
					_ = json.Unmarshal(frame.Params, &n)
					if u := n.Update.ToolCallUpdate; u != nil {
						if u.ToolCallId == "cmd" && (u.Status != nil && *u.Status == acp.ToolCallStatusPending || u.Title != nil || u.RawInput != nil) {
							t.Fatalf("input approval clobbered the running command: %s", frame.Params)
						}
						if u.ToolCallId == "approval:stdin-1" && u.Status != nil && *u.Status == acp.ToolCallStatusFailed {
							approvalFinished = true
						}
					}
				case "":
					if frame.Error != nil || !strings.Contains(string(frame.Result), `"end_turn"`) {
						t.Fatalf("prompt = %+v", frame)
					}
					if got := <-decision; got != "cancel" {
						t.Fatalf("backend decision = %v, want cancel", got)
					}
					if !approvalFinished {
						t.Fatal("input approval card was left pending")
					}
					return
				default:
					t.Fatalf("unexpected frame %+v", frame)
				}
			}
		})
	}
}

func TestResumeLoadAndForkReportPlanMode(t *testing.T) {
	planMode := map[string]any{"mode": "plan", "settings": map[string]any{"model": "m", "reasoning_effort": nil, "developer_instructions": nil}}
	collaboration := func(options []acp.SessionConfigOption) string {
		for _, option := range options {
			if option.Select != nil && option.Select.Id == collaborationModeConfigID {
				return string(option.Select.CurrentValue)
			}
		}
		return ""
	}
	for _, tt := range []struct {
		name           string
		mode           any
		localMode      string
		unmaterialized bool
		forkFails      bool
		want           string
		wantFork       string
	}{
		{name: "restored plan", mode: planMode, want: planCollaborationMode, wantFork: planCollaborationMode},
		{name: "fork cannot apply plan", mode: planMode, forkFails: true, want: planCollaborationMode, wantFork: defaultCollaborationMode},
		{name: "older server resets local plan", localMode: planCollaborationMode, want: defaultCollaborationMode, wantFork: defaultCollaborationMode},
		{name: "restored default overrides local plan", mode: map[string]any{"mode": "default"}, localMode: planCollaborationMode, want: defaultCollaborationMode, wantFork: defaultCollaborationMode},
		{name: "unprompted plan", unmaterialized: true, localMode: planCollaborationMode, want: planCollaborationMode, wantFork: planCollaborationMode},
		{name: "unprompted default", unmaterialized: true, localMode: defaultCollaborationMode, want: defaultCollaborationMode, wantFork: defaultCollaborationMode},
		{name: "unprompted without local session", unmaterialized: true, want: defaultCollaborationMode, wantFork: defaultCollaborationMode},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var settings []string
			var mu sync.Mutex
			agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
				switch req.Method {
				case "thread/resume":
					if tt.unmaterialized {
						s.respondError(req, -32600, "no rollout found for thread id thread-1")
						return
					}
					s.respond(req, map[string]any{"thread": map[string]any{"id": "thread-1"}, "model": "m", "collaborationMode": tt.mode})
				case "thread/read":
					s.respond(req, map[string]any{"thread": map[string]any{"id": "thread-1", "model": "m", "turns": []any{}}})
				case "thread/fork":
					s.respond(req, map[string]any{"thread": map[string]any{"id": "fork-1"}, "model": "m"})
				case "thread/settings/update":
					var p threadSettingsUpdateParams
					_ = json.Unmarshal(req.Params, &p)
					mu.Lock()
					settings = append(settings, p.ThreadID+":"+p.CollaborationMode.Mode)
					mu.Unlock()
					if tt.forkFails {
						s.respondError(req, -32600, "settings unavailable")
						return
					}
					s.respond(req, map[string]any{})
				default:
					s.respond(req, map[string]any{})
				}
			})
			ctx := context.Background()
			if tt.localMode != "" {
				agent.registerSession("thread-1", "m", "", tt.localMode, nil)
			}
			resumed, err := agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "thread-1", Cwd: "/work"})
			if err != nil || collaboration(resumed.ConfigOptions) != tt.want {
				t.Fatalf("resume = %#v, %v", resumed.ConfigOptions, err)
			}
			if tt.localMode != "" {
				agent.registerSession("thread-1", "m", "", tt.localMode, nil)
			}
			loaded, err := agent.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "thread-1", Cwd: "/work", McpServers: []acp.McpServer{}})
			if err != nil || collaboration(loaded.ConfigOptions) != tt.want {
				t.Fatalf("load = %#v, %v", loaded.ConfigOptions, err)
			}
			if s := agent.lookup("thread-1"); s == nil || s.collaborationMode != tt.want {
				t.Fatalf("loaded session = %#v", s)
			}
			forked, err := agent.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{SessionId: "thread-1", Cwd: "/work"})
			if err != nil {
				t.Fatal(err)
			}
			var forkMode string
			for _, option := range forked.ConfigOptions {
				if option.Select != nil && option.Select.Id == collaborationModeConfigID {
					forkMode = string(option.Select.CurrentValue)
				}
			}
			if forkMode != tt.wantFork {
				t.Fatalf("fork collaboration mode = %q, want %q", forkMode, tt.wantFork)
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.want == planCollaborationMode && (len(settings) != 1 || settings[0] != "fork-1:plan") {
				t.Fatalf("settings updates = %v, want plan applied to the fork only", settings)
			}
			if tt.want == defaultCollaborationMode && len(settings) != 0 {
				t.Fatalf("settings updates = %v", settings)
			}
		})
	}
}
