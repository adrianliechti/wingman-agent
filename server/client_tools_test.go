package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const clientToolName = "editor_selection"

const clientToolInput = `{"query":"selection","line":3}`

// clientToolModel calls the client's tool once, then answers with its output.
type clientToolModel struct {
	mu    sync.Mutex
	tools map[string][]string // the descriptions of the tools the model was offered, by name
}

func (m *clientToolModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6-sol"}]}`)
		return
	}
	var request struct {
		Tools []struct{ Name, Description string }
		Input []struct {
			Type   string
			Output json.RawMessage
		}
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &request)
	offered, output := map[string][]string{}, ""
	for _, tool := range request.Tools {
		offered[tool.Name] = append(offered[tool.Name], tool.Description)
	}
	for _, item := range request.Input {
		if item.Type == "function_call_output" {
			output = string(item.Output)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if _, ok := offered[clientToolName]; ok && output == "" {
		m.mu.Lock()
		m.tools = offered
		m.mu.Unlock()
		arguments, _ := json.Marshal(clientToolInput)
		fmt.Fprint(w, `data: {"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"`+clientToolName+`","arguments":`+string(arguments)+`,"status":"completed"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.completed","sequence_number":2,"response":{"usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\ndata: [DONE]\n\n")
		return
	}
	text, _ := json.Marshal("The model read " + output)
	fmt.Fprintf(w, `data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"answer","output_index":0,"content_index":0,"delta":%s}`+"\n\n", text)
	fmt.Fprintf(w, `data: {"type":"response.completed","sequence_number":2,"response":{"output":[{"type":"message","id":"answer","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":%s,"annotations":[]}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\ndata: [DONE]\n\n", text)
}

type clientToolPeer struct {
	t      *testing.T
	client *sdk.Client
	id     string
	seq    int64
	chat   *types.ChatState
	events *sdk.Subscription
}

func (p *clientToolPeer) dispatch(action types.StateAction) types.ActionEnvelope {
	p.t.Helper()
	p.seq++
	seq := p.seq
	if err := p.client.Notify(p.t.Context(), "dispatchAction", types.DispatchActionParams{Channel: p.chat.Resource, ClientSeq: seq, Action: action}); err != nil {
		p.t.Fatal(err)
	}
	return p.until(func(e types.ActionEnvelope) bool {
		return e.Origin != nil && e.Origin.ClientId == p.id && e.Origin.ClientSeq == seq
	})
}

func (p *clientToolPeer) until(predicate func(types.ActionEnvelope) bool) types.ActionEnvelope {
	p.t.Helper()
	return until(p.t, p.chat, p.events, predicate)
}

// call is the client's tool call as the peer's mirror holds it.
func (p *clientToolPeer) call() types.ToolCallState {
	parts := []types.ResponsePart{}
	if p.chat.ActiveTurn != nil {
		parts = p.chat.ActiveTurn.ResponseParts
	} else if n := len(p.chat.Turns); n > 0 {
		parts = p.chat.Turns[n-1].ResponseParts
	}
	for _, part := range parts {
		if tool, ok := part.Value.(*types.ToolCallResponsePart); ok {
			return tool.ToolCall
		}
	}
	return types.ToolCallState{}
}

// clientToolTurn starts a turn in which the model calls the owner's tool, and
// returns once the call waits for the owner.
func clientToolTurn(t *testing.T) (model *clientToolModel, app *Server, owner, observer *clientToolPeer, session string) {
	t.Helper()
	testenv.WingmanHome(t)
	model = &clientToolModel{}
	gateway := httptest.NewServer(model)
	t.Cleanup(gateway.Close)
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-6-sol")
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)

	session = "ahp-session:/client-tools"
	peer := func(id string) *clientToolPeer {
		client := protocolTestClient(t, endpoint, "")
		if _, err := client.Initialize(ctx, id, []string{"1.1.0"}, []string{types.RootResourceURI}); err != nil {
			t.Fatal(err)
		}
		return &clientToolPeer{t: t, client: client, id: id}
	}
	owner, observer = peer("owner"), peer("observer")
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"line":{"type":"integer"}}}`)
	tools := []types.ToolDefinition{
		{Name: clientToolName, Description: new("Returns the editor's selection."), InputSchema: &schema, Annotations: &types.ToolAnnotations{ReadOnlyHint: new(true)}},
		{Name: "read", Description: new("The client's file reader.")},
	}
	if err := owner.client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("wingman"), WorkingDirectories: []string{app.Project.directoryURI()},
		ActiveClient: &types.SessionActiveClient{ClientId: owner.id, Tools: tools}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*clientToolPeer{owner, observer} {
		p.chat, p.events = subscribeChat(t, p.client, ahp.ChatURI(session))
	}
	started := types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "turn", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "what is selected?", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
	owner.seq++
	if err := owner.client.Notify(ctx, "dispatchAction", types.DispatchActionParams{Channel: owner.chat.Resource, ClientSeq: owner.seq, Action: started}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*clientToolPeer{owner, observer} {
		p.until(func(types.ActionEnvelope) bool {
			_, running := p.call().Value.(*types.ToolCallRunningState)
			return running
		})
		running := p.call().Value.(*types.ToolCallRunningState)
		contributor, _ := running.Contributor.Value.(*types.ToolCallClientContributor)
		if contributor == nil || contributor.ClientId != owner.id || running.ToolName != clientToolName {
			t.Fatalf("%s: the call is not the owner's: %+v", p.id, running)
		}
		// The owner runs the tool with the model's input, not the transcript's shortened display of it.
		if running.ToolInput == nil || running.ToolInput.Inline == nil || *running.ToolInput.Inline != clientToolInput {
			t.Fatalf("%s: tool input: %s", p.id, clientToolJSON(t, running.ToolInput))
		}
	}
	return model, app, owner, observer, session
}

func clientToolText(text string) []types.ToolResultContent {
	return []types.ToolResultContent{{Value: &types.ToolResultTextContent{Type: types.ToolResultContentTypeText, Text: text}}}
}

func clientToolJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAHPClientToolWaitsForItsOwner(t *testing.T) {
	model, app, owner, observer, session := clientToolTurn(t)

	// A client's tool replaces the agent's own of the same name.
	model.mu.Lock()
	read := model.tools["read"]
	model.mu.Unlock()
	if len(read) != 1 || read[0] != "The client's file reader." {
		t.Fatalf("the model was offered these read tools: %q", read)
	}

	// The owner finds the call in the session, without following the chat.
	app.Project.host.mu.Lock()
	requests := app.Project.host.sessions[session].state.InputNeeded
	app.Project.host.mu.Unlock()
	request, _ := requests[0].Value.(*types.SessionToolClientExecutionRequest)
	if len(requests) != 1 || request == nil || request.ClientId != owner.id || request.Chat != owner.chat.Resource || request.TurnId != "turn" {
		t.Fatalf("the session does not ask the owner to run the call: %s", clientToolJSON(t, requests))
	}
	id := request.ToolCall.ToolCallId

	// Nobody else reports on the owner's call.
	spoofed := types.ToolCallResult{Success: true, PastTenseMessage: types.NewStringOrMarkdownPlain("Spoofed"), Content: clientToolText("not the owner")}
	for _, action := range []types.StateAction{
		{Value: &types.ChatToolCallCompleteAction{Type: types.ActionTypeChatToolCallComplete, TurnId: "turn", ToolCallId: id, Result: spoofed}},
		{Value: &types.ChatToolCallContentChangedAction{Type: types.ActionTypeChatToolCallContentChanged, TurnId: "turn", ToolCallId: id, Content: spoofed.Content}},
	} {
		if echo := observer.dispatch(action); echo.RejectionReason == nil {
			t.Fatalf("a client reported on another client's call: %+v", echo)
		}
	}

	// What the owner shows while it works replaces what it showed before.
	for _, text := range []string{"looking", "found it"} {
		echo := owner.dispatch(types.StateAction{Value: &types.ChatToolCallContentChangedAction{Type: types.ActionTypeChatToolCallContentChanged, TurnId: "turn", ToolCallId: id, Content: clientToolText(text)}})
		if echo.RejectionReason != nil {
			t.Fatalf("the owner's progress was rejected: %s", *echo.RejectionReason)
		}
		observer.until(func(e types.ActionEnvelope) bool { return e.ServerSeq == echo.ServerSeq })
		for _, p := range []*clientToolPeer{owner, observer} {
			running, _ := p.call().Value.(*types.ToolCallRunningState)
			if running == nil || !reflect.DeepEqual(running.Content, clientToolText(text)) {
				t.Fatalf("%s: running content: %s", p.id, clientToolJSON(t, p.call()))
			}
		}
	}

	result := types.ToolCallResult{Success: true, PastTenseMessage: types.NewStringOrMarkdownPlain("Read the selection"), Content: clientToolText("line 3 is selected"),
		StructuredContent: map[string]json.RawMessage{"line": json.RawMessage("3")}}
	if echo := owner.dispatch(types.StateAction{Value: &types.ChatToolCallCompleteAction{Type: types.ActionTypeChatToolCallComplete, TurnId: "turn", ToolCallId: id, Result: result}}); echo.RejectionReason != nil {
		t.Fatalf("the owner's result was rejected: %s", *echo.RejectionReason)
	}
	for _, p := range []*clientToolPeer{owner, observer} {
		p.until(func(e types.ActionEnvelope) bool {
			_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
			return complete
		})
		completed, _ := p.call().Value.(*types.ToolCallCompletedState)
		if completed == nil || !completed.Success || !reflect.DeepEqual(completed.Content, result.Content) || string(completed.StructuredContent["line"]) != "3" {
			t.Fatalf("%s: completed call: %s", p.id, clientToolJSON(t, p.call()))
		}
	}
	turn := owner.chat.Turns[0]
	if text := clientToolJSON(t, turn.ResponseParts); !strings.Contains(text, "The model read") || !strings.Contains(text, "line 3 is selected") {
		t.Fatalf("the model did not continue with the client's result: %s", text)
	}

	// The host keeps the call as its clients reduced it.
	fresh, _ := subscribeChat(t, observer.client, owner.chat.Resource)
	if want, got := clientToolJSON(t, turn.ResponseParts), clientToolJSON(t, fresh.Turns[0].ResponseParts); want != got {
		t.Fatalf("snapshot differs from the mirror:\n%s\n%s", got, want)
	}
	app.Project.host.mu.Lock()
	requests = app.Project.host.sessions[session].state.InputNeeded
	app.Project.host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("the session still asks for the call: %s", clientToolJSON(t, requests))
	}
}

func TestAHPClientToolFailsWhenItsOwnerLeaves(t *testing.T) {
	_, _, owner, observer, _ := clientToolTurn(t)
	if err := owner.client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	observer.until(func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})
	completed, _ := observer.call().Value.(*types.ToolCallCompletedState)
	if completed == nil || completed.Success || completed.Error == nil {
		t.Fatalf("the abandoned call did not fail: %s", clientToolJSON(t, observer.call()))
	}
	if text := clientToolJSON(t, observer.chat.Turns[0].ResponseParts); !strings.Contains(text, "The model read") || !strings.Contains(text, "disconnected") {
		t.Fatalf("the model did not continue after the failure: %s", text)
	}
}

func TestAHPClientToolResultReadsEmbeddedText(t *testing.T) {
	embedded := func(contentType, data string) types.ToolResultContent {
		return types.ToolResultContent{Value: &types.ToolResultEmbeddedResourceContent{Type: types.ToolResultContentTypeEmbeddedResource, ContentType: contentType, Data: base64.StdEncoding.EncodeToString([]byte(data))}}
	}
	result := clientToolResult(types.ToolCallResult{Success: true, PastTenseMessage: types.NewStringOrMarkdownPlain("Read the selection"), Content: []types.ToolResultContent{
		embedded("text/plain; charset=utf-8", "line 3 is selected"),
		embedded("application/zip", "PK"),
	}})
	if got, want := result.Content.Text(), "line 3 is selected\n[unsupported application/zip content]"; got != want || result.IsError {
		t.Fatalf("result = %q (error %v), want %q", got, result.IsError, want)
	}
}
