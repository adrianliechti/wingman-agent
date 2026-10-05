package codex

import (
	"context"
	"encoding/json"
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
			params := `{"sessionId":"locked-session","cwd":"/contract","mcpServers":[]}`
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

func TestUnrelatedThreadLockErrorsKeepTheirDiagnostics(t *testing.T) {
	agent, _ := scriptedAgent(t, func(s *contractAppServer, req rpcMessage) {
		if req.Method == "thread/resume" {
			s.respondError(req, -32603, "failed to acquire thread writer lock")
		} else {
			s.respond(req, map[string]any{})
		}
	})
	peer := newProtocolPeer(t, agent)
	peer.send(t, `"load"`, "session/load", `{"sessionId":"s","cwd":"/contract","mcpServers":[]}`)
	frame := peer.response(t, `"load"`)
	if frame.Error == nil || frame.Error.Code != -32603 || !strings.Contains(string(frame.Error.Data), "failed to acquire thread writer lock") {
		t.Fatalf("unrelated lock error = %+v", frame.Error)
	}
}
