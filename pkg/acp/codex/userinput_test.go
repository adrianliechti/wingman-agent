package codex

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestUserInputFormSchemaAndAnswers(t *testing.T) {
	questions := []userInputQuestion{
		{ID: "color", Header: "Color", Question: "Which color?", IsOther: true, Options: []userInputOption{{Label: "Red", Description: "Warm"}, {Label: "Blue"}}},
		{ID: "color_note", Question: "Existing field with the note name"},
		{ID: "name", Header: "Name only"},
		{ID: "done", Question: "Already offers other", IsOther: true, Options: []userInputOption{{Label: userInputOtherOption}}},
	}
	schema := userInputSchema(questions)
	if !reflect.DeepEqual(schema.Required, []string{"color", "color_note", "name", "done"}) {
		t.Fatalf("required = %v", schema.Required)
	}
	color := schema.Properties["color"].(map[string]any)
	if color["title"] != "Which color?" || color["description"] != "Color" {
		t.Fatalf("color field = %#v", color)
	}
	oneOf := color["oneOf"].([]any)
	if len(oneOf) != 3 || oneOf[0].(map[string]any)["description"] != "Warm" || oneOf[1].(map[string]any)["description"] != nil || oneOf[2].(map[string]any)["const"] != userInputOtherOption {
		t.Fatalf("color choices = %#v", oneOf)
	}
	note, ok := schema.Properties["color_note1"].(map[string]any)
	if !ok || note["title"] != "Additional answer or note" {
		t.Fatalf("colliding note field = %#v", schema.Properties)
	}
	if name := schema.Properties["name"].(map[string]any); name["title"] != "Name only" || name["oneOf"] != nil {
		t.Fatalf("free-text field = %#v", name)
	}
	if done := schema.Properties["done"].(map[string]any)["oneOf"].([]any); len(done) != 1 {
		t.Fatalf("duplicate other choice = %#v", done)
	}

	answers := userInputAnswers(questions, map[string]any{
		"color": userInputOtherOption, "color_note1": "  green  ", "color_note": "field answer",
		"name": "   ", "done": []any{"x", "y"},
	})
	want := map[string]userInputAnswer{
		"color":      {Answers: []string{userInputOtherOption, "user_note: green"}},
		"color_note": {Answers: []string{"field answer"}},
		"done":       {Answers: []string{"x", "y"}},
	}
	if !reflect.DeepEqual(answers.Answers, want) {
		t.Fatalf("answers = %#v", answers.Answers)
	}
	if b, _ := json.Marshal(emptyUserInputResponse()); string(b) != `{"answers":{}}` {
		t.Fatalf("empty response = %s", b)
	}
}

func TestUserInputRoundTripThroughACPElicitation(t *testing.T) {
	for _, tt := range []struct {
		name, turnID, reply, autoResolution string
		form                                bool
		want                                string
	}{
		{"accepted", "new-turn", `{"action":"accept","content":{"q":"Yes"}}`, "null", true, `{"answers":{"q":{"answers":["Yes"]}}}`},
		{"declined", "new-turn", `{"action":"decline"}`, "null", true, `{"answers":{}}`},
		{"auto-resolved", "new-turn", "", "20", true, `{"answers":{}}`},
		{"no form capability", "new-turn", "", "null", false, `{"answers":{}}`},
		{"stale turn", "old-turn", "", "null", true, `{"answers":{}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			answered := make(chan string, 1)
			session, client := faultBackend(t, func(conn net.Conn, request rpcMessage) {
				replyTurnStarted(conn, request)
				writeRPCMessage(conn, rpcMessage{ID: json.RawMessage(`"input"`), Method: "item/tool/requestUserInput", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"` + tt.turnID + `","itemId":"ask","questions":[{"id":"q","header":"Proceed","question":"Continue?","isOther":false,"isSecret":false,"options":[{"label":"Yes","description":""}]}],"autoResolutionMs":` + tt.autoResolution + `}`)})
				var reply rpcMessage
				_ = json.NewDecoder(conn).Decode(&reply)
				answered <- string(reply.Result)
				writeRPCMessage(conn, rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"new-turn","status":"completed"}}`)})
			})
			agent := newAgent(client, "default", "")
			agent.sessions[session.id] = session
			if tt.form {
				agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
			}
			peer := newProtocolPeer(t, agent)
			peer.send(t, `"prompt"`, "session/prompt", `{"sessionId":"thread-1","prompt":[{"type":"text","text":"Ask me"}]}`)
			for {
				frame := peer.next(t)
				switch frame.Method {
				case "elicitation/create":
					var params acp.UnstableCreateElicitationForm
					_ = json.Unmarshal(frame.Params, &params)
					if params.Meta["sessionId"] != "thread-1" || params.Meta["toolCallId"] != "ask" || params.RequestedSchema.Properties["q"] == nil {
						t.Fatalf("elicitation = %s", frame.Params)
					}
					if tt.reply != "" {
						peer.reply(t, frame.ID, tt.reply)
					}
				case "session/update", "$/cancel_request":
				case "":
					if string(frame.ID) != `"prompt"` || frame.Error != nil || !strings.Contains(string(frame.Result), "end_turn") {
						t.Fatalf("prompt = %+v", frame)
					}
					if got := <-answered; got != tt.want {
						t.Fatalf("answers = %s, want %s", got, tt.want)
					}
					return
				default:
					t.Fatalf("unexpected frame %+v", frame)
				}
			}
		})
	}
}
