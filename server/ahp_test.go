package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
)

func ahpTestServer(t *testing.T) (*Server, *backendRuntime, *protocolAgent, string) {
	t.Helper()
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, b, a := newProtocolServer(t)
	s.ahpHost()
	s.mux.Get("/ahp", s.handleAHP)
	s.handler = LocalHostOnly(http.NewCrossOriginProtection().Handler(s.checkInstance(s.mux)))
	httpServer := httptest.NewServer(s)
	t.Cleanup(httpServer.Close)
	return s, b, a, "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ahp"
}

func ahpTestClient(t *testing.T, endpoint, id string) *ahp.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	transport, err := ahpws.Connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	if id != "" {
		result, err := client.Initialize(ctx, id, []string{"1.0.0"}, []string{types.RootResourceURI})
		if err != nil {
			t.Fatal(err)
		}
		if result.ProtocolVersion != "1.0.0" || len(result.Snapshots) != 1 || result.Snapshots[0].State.Root == nil {
			t.Fatalf("bad handshake: %+v", result)
		}
	}
	return client
}

func ahpCreate(t *testing.T, client *ahp.Client, uri string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: uri, Provider: ahpPtr("one")}, nil); err != nil {
		t.Fatal(err)
	}
	result, sub, err := client.Subscribe(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if result.Snapshot == nil || result.Snapshot.State.Session == nil || result.Snapshot.State.Session.DefaultChat == nil {
		t.Fatalf("bad session: %+v", result)
	}
	return *result.Snapshot.State.Session.DefaultChat
}

func ahpSubscribeChat(t *testing.T, client *ahp.Client, uri string) (*types.ChatState, *ahp.Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, sub, err := client.Subscribe(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot == nil || result.Snapshot.State.Chat == nil {
		t.Fatalf("bad chat: %+v", result)
	}
	t.Cleanup(sub.Close)
	return result.Snapshot.State.Chat, sub
}

func ahpUntil(t *testing.T, state *types.ChatState, sub *ahp.Subscription, predicate func(types.ActionEnvelope) bool) types.ActionEnvelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed")
			}
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				if action.Envelope.RejectionReason == nil {
					ahp.ApplyActionToChat(state, action.Envelope.Action)
				}
				if predicate(action.Envelope) {
					return action.Envelope
				}
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for action; state=%+v", state)
		}
	}
}

func ahpSend(t *testing.T, client *ahp.Client, chat, turn string, seq int64) types.DispatchActionParams {
	t.Helper()
	p := types.DispatchActionParams{Channel: chat, ClientSeq: seq, Action: types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: turn, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "hello", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}}
	if err := client.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAHPClientStreamingSharedSessionAndReconnect(t *testing.T) {
	_, _, agent, endpoint := ahpTestServer(t)
	agent.gate = make(chan struct{})
	first := ahpTestClient(t, endpoint, "first")
	chat := ahpCreate(t, first, "ahp-session:/test")
	firstState, firstSub := ahpSubscribeChat(t, first, chat)
	second := ahpTestClient(t, endpoint, "second")
	secondState, secondSub := ahpSubscribeChat(t, second, chat)
	p := ahpSend(t, first, chat, "turn-1", 1)
	streamed := func(e types.ActionEnvelope) bool {
		return firstState.ActiveTurn != nil && len(firstState.ActiveTurn.ResponseParts) > 0
	}
	ahpUntil(t, firstState, firstSub, streamed)
	ahpUntil(t, secondState, secondSub, func(types.ActionEnvelope) bool {
		return secondState.ActiveTurn != nil && len(secondState.ActiveTurn.ResponseParts) > 0
	})
	if !reflect.DeepEqual(firstState, secondState) {
		t.Fatalf("clients diverged\n%+v\n%+v", firstState, secondState)
	}
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	ack := ahpUntil(t, firstState, firstSub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 1 })
	if ack.RejectionReason != nil {
		t.Fatalf("retry rejected: %v", *ack.RejectionReason)
	}
	if agent.sent.Load() != 1 {
		t.Fatal("duplicate execution")
	}
	_ = first.Shutdown(t.Context())
	close(agent.gate)
	ahpUntil(t, secondState, secondSub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	reconnected := ahpTestClient(t, endpoint, "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := reconnected.Reconnect(ctx, "first", ack.ServerSeq, []string{types.RootResourceURI, chat})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := result.Value.(*types.ReconnectSnapshotResult)
	if !ok {
		t.Fatalf("expected snapshots: %+v", result)
	}
	var recovered *types.ChatState
	for _, snap := range snapshot.Snapshots {
		if snap.Resource == chat {
			recovered = snap.State.Chat
		}
	}
	if recovered == nil || recovered.ActiveTurn != nil || len(recovered.Turns) != 1 {
		t.Fatalf("bad recovery: %+v", recovered)
	}
	if recovered.Turns[0].Id != "turn-1" {
		t.Fatalf("turn identity changed: %+v", recovered.Turns[0])
	}
}

func TestAHPRejectBusySendAndCancel(t *testing.T) {
	_, _, agent, endpoint := ahpTestServer(t)
	agent.gate = make(chan struct{})
	client := ahpTestClient(t, endpoint, "cancel")
	chat := ahpCreate(t, client, "ahp-session:/cancel")
	state, sub := ahpSubscribeChat(t, client, chat)
	ahpSend(t, client, chat, "one", 1)
	ahpUntil(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	ahpSend(t, client, chat, "two", 2)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { return e.RejectionReason != nil })
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: chat, ClientSeq: 3, Action: types.StateAction{Value: &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "one"}}}); err != nil {
		t.Fatal(err)
	}
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 3 })
	if state.ActiveTurn != nil || len(state.Turns) != 1 || state.Turns[0].State != types.TurnStateCancelled {
		t.Fatalf("bad cancellation: %+v", state)
	}
	if agent.sent.Load() != 1 {
		t.Fatal("busy send executed")
	}
}

func TestAHPResourcesConfinedToWorkspace(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	root := t.TempDir()
	workspace, err := code.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	s.workspace = workspace
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	client := ahpTestClient(t, endpoint, "files")
	uri := func(p string) string { return (&url.URL{Scheme: "file", Path: p}).String() }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.ResourceRead(ctx, types.ResourceReadParams{Uri: uri(filepath.Join(root, "hello.txt"))})
	if err != nil || result.Data != "hello" {
		t.Fatalf("read failed: %+v %v", result, err)
	}
	for _, path := range []string{filepath.Join(outside, "secret"), filepath.Join(root, "escape", "secret")} {
		if _, err := client.ResourceRead(ctx, types.ResourceReadParams{Uri: uri(path)}); err == nil {
			t.Fatalf("read escaped workspace: %s", path)
		}
	}
}

func TestAHPInputQuestionWireTypes(t *testing.T) {
	p := &ahpPrompt{view: PromptView{ID: "input", Kind: PromptKindAsk, Message: "Choose", Fields: []tool.ElicitField{{Name: "choice", Type: "string", Enum: []string{"one", "two"}}}}}
	data, err := json.Marshal(ahpPromptPart(p))
	if err != nil {
		t.Fatal(err)
	}
	var part types.ResponsePart
	if err := json.Unmarshal(data, &part); err != nil {
		t.Fatal(err)
	}
	request := part.Value.(*types.InputRequestResponsePart).Request
	if _, ok := request.Questions[0].Value.(*types.ChatInputSingleSelectQuestion); !ok {
		t.Fatalf("question is not interoperable: %s", data)
	}
}

func TestAHPElicitationReplyAndDuplicateReceipt(t *testing.T) {
	_, b, agent, endpoint := ahpTestServer(t)
	agent.gate = make(chan struct{})
	client := ahpTestClient(t, endpoint, "questions")
	chat := ahpCreate(t, client, "ahp-session:/questions")
	state, sub := ahpSubscribeChat(t, client, chat)
	ahpSend(t, client, chat, "turn", 1)
	ahpUntil(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	replies := make(chan tool.ElicitResult, 1)
	go func() {
		result, _ := b.Elicit(code.WithSessionID(t.Context(), "session-1"), tool.ElicitRequest{Message: "Choose", Fields: []tool.ElicitField{{Name: "option", Type: "string", Enum: []string{"one", "two"}, Required: true}}})
		replies <- result
	}()
	var requestID string
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool {
		if input, ok := e.Action.Value.(*types.ChatInputRequestedAction); ok {
			requestID = input.Request.Id
			return true
		}
		return false
	})
	answer := types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatInputCompletedAction{Type: types.ActionTypeChatInputCompleted, RequestId: requestID, Response: types.ChatInputResponseKindAccept, Answers: map[string]types.ChatInputAnswer{"option": {Value: &types.ChatInputAnswered{State: types.ChatInputAnswerStateSubmitted, Value: types.ChatInputAnswerValue{Value: &types.ChatInputSelectedAnswerValue{Kind: types.ChatInputAnswerValueKindSelected, Value: "two"}}}}}}}}
	if err := client.Notify(t.Context(), "dispatchAction", answer); err != nil {
		t.Fatal(err)
	}
	ack := ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 2 })
	if ack.RejectionReason != nil {
		t.Fatal(*ack.RejectionReason)
	}
	select {
	case reply := <-replies:
		if reply.Action != tool.ElicitAccept || reply.Content["option"] != "two" {
			t.Fatalf("bad reply: %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("elicitation blocked")
	}
	if err := client.Notify(t.Context(), "dispatchAction", answer); err != nil {
		t.Fatal(err)
	}
	retry := ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 2 })
	if retry.ServerSeq != ack.ServerSeq || retry.RejectionReason != nil {
		t.Fatal("duplicate approval was not acknowledged consistently")
	}
	close(agent.gate)
}

func TestAHPPersistedSessionURI(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	client := ahpTestClient(t, endpoint, "persistence")
	uri := "ahp-session:/durable"
	chat := ahpCreate(t, client, uri)
	other, _, agent := newProtocolServer(t)
	h := other.ahpHost()
	a, err := h.ensureSession(chat)
	if err != nil {
		t.Fatal(err)
	}
	if a.resource != uri || a.controller.ref.SessionID != "session-1" || agent.loaded.Load() != 1 {
		t.Fatalf("session alias did not recover: %+v", a)
	}
	if s.scope.WorkspaceID != other.scope.WorkspaceID {
		t.Fatal("fixture must share the workspace namespace")
	}
}

func TestAHPBuiltinAgentRoundTrip(t *testing.T) {
	testenv.WingmanHome(t)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6-sol"}]}`)
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"answer\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello from Wingman\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"answer\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello from Wingman\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-6-sol")
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app)
	defer server.Close()
	client := ahpTestClient(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ahp", "builtin")
	var config types.ResolveSessionConfigResult
	if err := client.Request(t.Context(), "resolveSessionConfig", types.ResolveSessionConfigParams{Channel: types.RootResourceURI, Provider: ahpPtr("wingman")}, &config); err != nil {
		t.Fatal(err)
	}
	if string(config.Values["model"]) != `"gpt-6-sol"` {
		t.Fatalf("model configuration missing: %+v", config)
	}
	uri := "ahp-session:/builtin"
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: uri, Provider: ahpPtr("wingman"), WorkingDirectories: []string{app.ahp.directoryURI()}, Config: config.Values}, nil); err != nil {
		t.Fatal(err)
	}
	state, sub := ahpSubscribeChat(t, client, ahpChatURI(uri))
	ahpSend(t, client, state.Resource, "builtin-turn", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})
	if len(state.Turns) != 1 || state.Turns[0].Id != "builtin-turn" {
		t.Fatalf("unexpected history: %+v", state)
	}
	var text string
	for _, part := range state.Turns[0].ResponseParts {
		if markdown, ok := part.Value.(*types.MarkdownResponsePart); ok {
			text += markdown.Content
		}
	}
	if text != "Hello from Wingman" {
		t.Fatalf("unexpected response: %q", text)
	}
}

func TestAHPRejectUnsupportedVersion(t *testing.T) {
	_, _, _, endpoint := ahpTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := ahpTestClient(t, endpoint, "")
	_, err := client.Initialize(ctx, "old", []string{"99.0.0"}, nil)
	if err == nil {
		t.Fatal("incompatible version accepted")
	}
}

func TestAHPVSCodeDesktopHandshake(t *testing.T) {
	_, _, _, endpoint := ahpTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{
		"Origin":         []string{"vscode-file://vscode-app"},
		"Sec-Fetch-Site": []string{"cross-site"},
		"Sec-Fetch-Mode": []string{"websocket"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"channel":"ahp-root://","clientId":"vscode-desktop","protocolVersions":["1.0.0"],"initialSubscriptions":["ahp-root://"]}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result types.InitializeResult `json:"result"`
		Error  *ahpRPCError           `json:"error"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil || reply.Result.ProtocolVersion != "1.0.0" || len(reply.Result.Snapshots) != 1 {
		t.Fatalf("desktop initialization failed: %s", data)
	}
	var resources struct {
		Platform string `json:"platform"`
	}
	root := reply.Result.Snapshots[0].State.Root
	if root == nil || json.Unmarshal(root.Meta["vscode.agentHost.resources"], &resources) != nil || resources.Platform == "" {
		t.Fatal("VS Code host platform metadata missing")
	}
}

func TestAHPNotFoundErrors(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	workspace, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.workspace = workspace
	client := ahpTestClient(t, endpoint, "missing-resources")
	for _, method := range []string{"resourceRead", "resourceResolve", "resourceList"} {
		err := client.Request(t.Context(), method, map[string]any{"channel": types.RootResourceURI, "uri": s.ahp.directoryURI() + "/missing"}, nil)
		var rpc *ahp.RPCError
		if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeNotFound {
			t.Fatalf("%s: expected NotFound, got %v", method, err)
		}
	}
	_, _, err = client.Subscribe(t.Context(), "ahp-session:/missing")
	var rpc *ahp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeSessionNotFound {
		t.Fatalf("expected SessionNotFound, got %v", err)
	}
}

func TestAHPOriginPolicy(t *testing.T) {
	_, _, _, endpoint := ahpTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	host, _ := url.Parse(endpoint)
	for origin, allowed := range map[string]bool{
		"": true, "http://" + host.Host: true, "vscode-file://vscode-app": true, "vscode-webview://panel": true, "custom-app://desktop": true,
		"null": false, "https://client.example": false, "http://localhost:3000": false, "file://": false,
	} {
		t.Run(origin, func(t *testing.T) {
			headers := http.Header{"Sec-Fetch-Site": []string{"cross-site"}, "Sec-Fetch-Mode": []string{"websocket"}}
			if origin != "" {
				headers.Set("Origin", origin)
			}
			conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: headers})
			if allowed != (err == nil) {
				t.Fatalf("allowed=%v, err=%v", allowed, err)
			}
			if conn != nil {
				conn.CloseNow()
			}
		})
	}
}
