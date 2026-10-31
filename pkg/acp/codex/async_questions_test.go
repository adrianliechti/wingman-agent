package codex

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestAsyncQuestionSteersIntoItsOriginalTurn(t *testing.T) {
	steered := make(chan turnSteerParams, 1)
	session, client := faultBackend(t, func(conn net.Conn, request rpcMessage) {
		replyTurnStarted(conn, request)
		writeRPCMessage(conn, rpcMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"type":"agentMessage","id":"ask-1","delivery":"async","text":"Which environment?","questions":[{"title":"Which environment?","options":["Staging","Production"]},{"title":"Optional note"}]}}`)})
		var reply rpcMessage
		_ = json.NewDecoder(conn).Decode(&reply)
		if reply.Method != "turn/steer" {
			t.Errorf("async answer method = %q", reply.Method)
		}
		var params turnSteerParams
		_ = json.Unmarshal(reply.Params, &params)
		steered <- params
		writeRPCMessage(conn, rpcMessage{ID: reply.ID, Result: json.RawMessage(`{}`)})
		writeRPCMessage(conn, rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"new-turn","status":"completed"}}`)})
	})
	agent := newAgent(client, "default", "")
	agent.sessions[session.id] = session
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	peer := newProtocolPeer(t, agent)
	peer.send(t, `"prompt"`, "session/prompt", `{"sessionId":"thread-1","prompt":[{"type":"text","text":"Ask while working"}]}`)
	for {
		frame := peer.next(t)
		if frame.Method == "elicitation/create" {
			var form acp.UnstableCreateElicitationForm
			if json.Unmarshal(frame.Params, &form) != nil || len(form.RequestedSchema.Required) != 0 || form.Meta["sessionId"] != "thread-1" {
				t.Fatalf("async form = %s", frame.Params)
			}
			peer.reply(t, frame.ID, `{"action":"accept","content":{"question_0":"Staging"}}`)
		}
		if string(frame.ID) == `"prompt"` && frame.Method == "" {
			if frame.Error != nil {
				t.Fatal(frame.Error)
			}
			break
		}
	}
	params := <-steered
	if params.ExpectedTurnID != "new-turn" || params.ClientUserMessageID == "" {
		t.Fatalf("steer scope = %+v", params)
	}
	input, _ := json.Marshal(params.Input)
	if !strings.Contains(string(input), "questionItemId") || !strings.Contains(string(input), "Staging") {
		t.Fatalf("steered input = %s", input)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(input, &blocks)
	if len(blocks) != 1 || renderQuestionReply(blocks[0].Text) != "> Which environment?\n\nStaging" {
		t.Fatalf("replayed answer = %v", blocks)
	}
}

func TestAsyncQuestionClosesWhenItsTurnFinishes(t *testing.T) {
	finish := make(chan struct{})
	session, client := faultBackend(t, func(conn net.Conn, request rpcMessage) {
		replyTurnStarted(conn, request)
		writeRPCMessage(conn, rpcMessage{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"new-turn","item":{"type":"agentMessage","id":"ask","delivery":"async","questions":[{"title":"Anything else?"}]}}`)})
		<-finish
		writeRPCMessage(conn, rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"new-turn","status":"completed"}}`)})
	})
	agent := newAgent(client, "default", "")
	agent.sessions[session.id] = session
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	peer := newProtocolPeer(t, agent)
	peer.send(t, `"prompt"`, "session/prompt", `{"sessionId":"thread-1","prompt":[{"type":"text","text":"Ask"}]}`)
	var promptDone, cancelled bool
	for !promptDone || !cancelled {
		frame := peer.next(t)
		if frame.Method == "elicitation/create" {
			close(finish)
		}
		if frame.Method == "$/cancel_request" {
			cancelled = true
		}
		if string(frame.ID) == `"prompt"` && frame.Method == "" {
			promptDone = true
			if frame.Error != nil {
				t.Fatal(frame.Error)
			}
		}
	}
}
