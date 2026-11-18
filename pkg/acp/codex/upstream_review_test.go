package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestSessionInUseReportsActionableErrorAndCanRetry(t *testing.T) {
	for _, method := range []string{"session/resume", "session/load"} {
		t.Run(method, func(t *testing.T) {
			var held atomic.Bool
			held.Store(true)
			agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
				if req.Method == "thread/resume" && held.Load() {
					s.respondError(req, -32600, "thread locked-session already has an active writer")
					return
				}
				switch req.Method {
				case "thread/resume":
					s.respond(req, threadResumeResponse{Thread: threadInfo{ID: "locked-session"}, Model: "default"})
				case "thread/read":
					s.respond(req, threadReadResponse{Thread: threadInfo{ID: "locked-session", HistoryMode: "legacy"}})
				default:
					s.respond(req, map[string]any{})
				}
			})
			peer := newProtocolPeer(t, agent)
			params := `{"sessionId":"locked-session","cwd":` + contractCwdJSON + `,"mcpServers":[]}`
			peer.send(t, `"held"`, method, params)
			frame := peer.response(t, `"held"`)
			if frame.Error == nil || frame.Error.Code != -32600 || !strings.Contains(frame.Error.Message, "another Codex client") {
				t.Fatalf("session in use error = %+v", frame.Error)
			}
			var data map[string]any
			if json.Unmarshal(frame.Error.Data, &data) != nil || data["reason"] != "thread_active_writer" || data["threadId"] != "locked-session" {
				t.Fatalf("error data = %s", frame.Error.Data)
			}
			if agent.lookup("locked-session") != nil {
				t.Fatal("failed resume installed a session")
			}
			held.Store(false)
			peer.send(t, `"retry"`, method, params)
			if frame := peer.response(t, `"retry"`); frame.Error != nil {
				t.Fatalf("retry after lock released: %+v", frame.Error)
			}
		})
	}
}

func TestServiceErrorEnvelopeIsReadableInTurnFailure(t *testing.T) {
	const service = `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"This model is unavailable.","code":null,"param":"model"}}`
	a := newAgent(&codexClient{}, "default", "")
	peer := newProtocolPeer(t, a)
	d := newEventDispatcher(context.Background(), a.connection(), "s")
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": service}, "willRetry": false})
	d.handle("error", raw)
	var notification acp.SessionNotification
	frame := peer.next(t)
	if json.Unmarshal(frame.Params, &notification) != nil || notification.Update.AgentMessageChunk == nil || notification.Update.AgentMessageChunk.Content.Text.Text != "This model is unavailable.\n\n" {
		t.Fatalf("error update = %s", frame.Params)
	}
	if err := (&turnError{Message: service, AdditionalDetails: "choose another model"}).Error(); err != "This model is unavailable.: choose another model" {
		t.Fatalf("failure message = %q", err)
	}
}

func TestServiceErrorsPreserveUnrecognizedDiagnostics(t *testing.T) {
	for _, text := range []string{
		"plain failure", "{invalid", "null",
		`{"error":{"message":"keep me"}}`,
		`{"type":"error","status":400,"custom":true,"error":{"type":"server_error","message":"keep me"}}`,
		`{"type":"error","status":200,"error":{"type":"server_error","message":"keep me"}}`,
		`{"type":"error","status":400.5,"error":{"type":"server_error","message":"keep me"}}`,
		`{"type":"error","status":400,"error":{"type":"custom_error","message":"keep me"}}`,
		`{"type":"error","status":400,"error":{"type":"server_error","message":"keep me","param":{}}}`,
	} {
		if got := readableServiceErrorMessage(text); got != text {
			t.Errorf("diagnostics changed: %s -> %s", text, got)
		}
	}
}

func TestBlockingHooksSurfaceOnlyReasonsFromTheCurrentTurn(t *testing.T) {
	for _, variant := range []string{"notices", "legacy"} {
		t.Run(variant, func(t *testing.T) {
			agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
				if req.Method != "turn/start" {
					s.respond(req, map[string]any{})
					return
				}
				s.respond(req, turnStartResponse{Turn: turn{ID: "live", Status: "inProgress"}})
				hook := func(turnID, event, status string) {
					s.notify("hook/completed", map[string]any{"threadId": "s", "turnId": turnID, "run": map[string]any{
						"eventName": event, "status": status, "entries": []any{
							map[string]string{"kind": "context", "text": "internal model context"},
							map[string]string{"kind": "feedback", "text": "  Workspace policy  "},
						},
					}})
				}
				hook("old", "userPromptSubmit", "blocked")
				hook("live", "preToolUse", "completed")
				hook("live", "preToolUse", "blocked")
				hook("live", "stop", "stopped")
				s.notify("turn/completed", map[string]any{"threadId": "s", "turn": turn{ID: "live", Status: "completed"}})
			})
			peer := newProtocolPeer(t, agent)
			caps := `{}`
			if variant == "notices" {
				caps = `{"session":{"notices":{}}}`
			}
			peer.send(t, `"init"`, "initialize", `{"protocolVersion":1,"clientCapabilities":`+caps+`}`)
			if reply := peer.response(t, `"init"`); reply.Error != nil {
				t.Fatal(reply.Error)
			}
			agent.registerSession("s", t.TempDir(), "default", "", "", nil)
			peer.send(t, `"p"`, "session/prompt", `{"sessionId":"s","prompt":[{"type":"text","text":"go"}]}`)
			var titles []string
			for {
				frame := peer.next(t)
				if string(frame.ID) == `"p"` {
					if frame.Error != nil {
						t.Fatal(frame.Error)
					}
					break
				}
				var n acp.SessionNotification
				if err := json.Unmarshal(frame.Params, &n); err != nil {
					t.Fatal(err)
				}
				if notice := n.Update.Notice; notice != nil {
					if variant != "notices" || notice.Severity != "warning" || notice.Description == nil || *notice.Description != "Workspace policy" {
						t.Fatalf("unexpected notice %+v", notice)
					}
					titles = append(titles, notice.Title)
				} else if chunk := n.Update.AgentMessageChunk; chunk != nil {
					title, ok := strings.CutSuffix(chunk.Content.Text.Text, ": Workspace policy\n\n")
					if variant != "legacy" || !ok {
						t.Fatalf("unexpected text %q", chunk.Content.Text.Text)
					}
					titles = append(titles, title)
				}
			}
			if !slices.Equal(titles, []string{"PreToolUse hook blocked the tool call", "Stop hook stopped the turn"}) {
				t.Fatalf("hook updates = %v", titles)
			}
		})
	}
}

func TestUnrelatedThreadLockErrorsKeepTheirDiagnostics(t *testing.T) {
	agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
		if req.Method == "thread/resume" {
			s.respondError(req, -32603, "failed to acquire thread writer lock")
		} else {
			s.respond(req, map[string]any{})
		}
	})
	peer := newProtocolPeer(t, agent)
	peer.send(t, `"load"`, "session/load", `{"sessionId":"s","cwd":`+contractCwdJSON+`,"mcpServers":[]}`)
	frame := peer.response(t, `"load"`)
	if frame.Error == nil || frame.Error.Code != -32603 || !strings.Contains(string(frame.Error.Data), "failed to acquire thread writer lock") {
		t.Fatalf("unrelated lock error = %+v", frame.Error)
	}
}

func TestListSessionsLeavesOutExecRunsAndTitlesOneLine(t *testing.T) {
	listParams := make(chan threadListParams, 1)
	agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
		var p threadListParams
		_ = json.Unmarshal(req.Params, &p)
		listParams <- p
		blank, named := " \n ", "  Fix\tthe   build "
		s.respond(req, threadListResponse{Data: []threadSummary{
			{ID: "blank-name", Name: &blank, Preview: "first line\n\nsecond line"},
			{ID: "named", Name: &named, Preview: "ignored"},
			{ID: "untitled", Preview: "   "},
		}})
	})
	resp, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p := <-listParams; !slices.Equal(p.SourceKinds, []string{"cli", "vscode", "appServer", "unknown"}) || !p.UseStateDBOnly {
		t.Fatalf("thread/list params = %+v", p)
	}
	var titles []string
	for _, session := range resp.Sessions {
		title := "<nil>"
		if session.Title != nil {
			title = *session.Title
		}
		titles = append(titles, title)
	}
	if want := []string{"first line second line", "Fix the build", "<nil>"}; !slices.Equal(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestConfigLoadErrorNamesDeletedWorkingDirectory(t *testing.T) {
	requests := []struct{ method, backend, params, failure string }{
		{"session/new", "thread/start", `{"cwd":` + contractCwdJSON + `,"mcpServers":[]}`,
			"failed to load configuration: No such file or directory (os error 2)"},
		{"session/resume", "thread/resume", `{"sessionId":"s","cwd":` + contractCwdJSON + `,"mcpServers":[]}`,
			"failed to load config: No such file or directory (os error 2)"},
	}
	for _, tt := range []struct {
		name    string
		change  func(dir string) error
		explain bool
	}{
		{"deleted", os.Remove, true},
		{"recreated", func(dir string) error {
			if err := os.Remove(dir); err != nil {
				return err
			}
			return os.Mkdir(dir, 0o755)
		}, true},
		{"present", func(string) error { return nil }, false},
	} {
		for _, req := range requests {
			t.Run(tt.name+"/"+req.method, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "workspace")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				agent, _ := scriptedAgent(t, func(s *contractAppServer, r rpcMessage) {
					if r.Method == req.backend {
						s.respondError(r, -32600, req.failure)
						return
					}
					s.respond(r, map[string]any{})
				})
				agent.codex.workDir = newWorkingDirectory(dir)
				if err := tt.change(dir); err != nil {
					t.Fatal(err)
				}
				peer := newProtocolPeer(t, agent)
				peer.send(t, `"open"`, req.method, req.params)
				frame := peer.response(t, `"open"`)
				if frame.Error == nil {
					t.Fatalf("%s succeeded", req.method)
				}
				data := string(frame.Error.Data)
				explained := strings.Contains(data, "working directory "+strings.Trim(jsonString(dir), `"`)+" was deleted or replaced")
				if explained != tt.explain || explained == strings.Contains(data, req.failure) {
					t.Fatalf("error data = %s", data)
				}
			})
		}
	}
}
