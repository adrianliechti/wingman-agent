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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/coder/websocket"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
)

func protocolTestServer(t *testing.T) (*Server, *backendRuntime, *protocolAgent, string) {
	t.Helper()
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, b, a := newProtocolServer(t)
	s.mux.Get("/ahp", s.Project.host.handleAHP)
	s.handler = LocalHostOnly(http.NewCrossOriginProtection().Handler(s.checkInstance(s.mux)))
	httpServer := httptest.NewServer(s)
	t.Cleanup(httpServer.Close)
	return s, b, a, "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ahp"
}

func protocolTestClient(t *testing.T, endpoint, id string) *sdk.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	transport, err := ahpws.Connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sdk.Connect(ctx, transport, sdk.DefaultConfig())
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

func create(t *testing.T, client *sdk.Client, uri string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: uri, Provider: new("one")}, nil); err != nil {
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

func subscribeChat(t *testing.T, client *sdk.Client, uri string) (*types.ChatState, *sdk.Subscription) {
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

func until(t *testing.T, state *types.ChatState, sub *sdk.Subscription, predicate func(types.ActionEnvelope) bool) types.ActionEnvelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed")
			}
			if action, ok := event.(sdk.SubscriptionEventAction); ok {
				if action.Envelope.RejectionReason == nil {
					sdk.ApplyActionToChat(state, action.Envelope.Action)
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

func send(t *testing.T, client *sdk.Client, chat, turn string, seq int64) types.DispatchActionParams {
	t.Helper()
	p := types.DispatchActionParams{Channel: chat, ClientSeq: seq, Action: types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: turn, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "hello", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}}
	if err := client.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAHPClientStreamingSharedSessionAndReconnect(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	first := protocolTestClient(t, endpoint, "first")
	chat := create(t, first, "ahp-session:/test")
	firstState, firstSub := subscribeChat(t, first, chat)
	second := protocolTestClient(t, endpoint, "second")
	secondState, secondSub := subscribeChat(t, second, chat)
	p := send(t, first, chat, "turn-1", 1)
	streamed := func(e types.ActionEnvelope) bool {
		return firstState.ActiveTurn != nil && len(firstState.ActiveTurn.ResponseParts) > 0
	}
	until(t, firstState, firstSub, streamed)
	until(t, secondState, secondSub, func(types.ActionEnvelope) bool {
		return secondState.ActiveTurn != nil && len(secondState.ActiveTurn.ResponseParts) > 0
	})
	if !reflect.DeepEqual(firstState, secondState) {
		t.Fatalf("clients diverged\n%+v\n%+v", firstState, secondState)
	}
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	ack := until(t, firstState, firstSub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 1 })
	if ack.RejectionReason != nil {
		t.Fatalf("retry rejected: %v", *ack.RejectionReason)
	}
	if agent.sent.Load() != 1 {
		t.Fatal("duplicate execution")
	}
	_ = first.Shutdown(t.Context())
	close(agent.gate)
	until(t, secondState, secondSub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	reconnected := protocolTestClient(t, endpoint, "")
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
	_, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "cancel")
	chat := create(t, client, "ahp-session:/cancel")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "one", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	send(t, client, chat, "two", 2)
	until(t, state, sub, func(e types.ActionEnvelope) bool { return e.RejectionReason != nil })
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: chat, ClientSeq: 3, Action: types.StateAction{Value: &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "one"}}}); err != nil {
		t.Fatal(err)
	}
	until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 3 })
	if state.ActiveTurn != nil || len(state.Turns) != 1 || state.Turns[0].State != types.TurnStateCancelled {
		t.Fatalf("bad cancellation: %+v", state)
	}
	if agent.sent.Load() != 1 {
		t.Fatal("busy send executed")
	}
}

func TestAHPResourcesConfinedToWorkspace(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	root := t.TempDir()
	workspace, err := code.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	s.Project.workspace = workspace
	s.Project = webProject(s, root)
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
	client := protocolTestClient(t, endpoint, "files")
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
	p := &prompt{view: PromptView{ID: "input", Kind: PromptKindAsk, Message: "Choose", Fields: []tool.ElicitField{{Name: "choice", Type: "string", Enum: []string{"one", "two"}}}}}
	data, err := json.Marshal(promptPart(p))
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
	_, b, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "questions")
	chat := create(t, client, "ahp-session:/questions")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	replies := make(chan tool.ElicitResult, 1)
	go func() {
		result, _ := b.Elicit(code.WithSessionID(t.Context(), "session-1"), tool.ElicitRequest{Message: "Choose", Fields: []tool.ElicitField{{Name: "option", Type: "string", Enum: []string{"one", "two"}, Required: true}}})
		replies <- result
	}()
	var requestID string
	until(t, state, sub, func(e types.ActionEnvelope) bool {
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
	ack := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 2 })
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
	retry := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 2 })
	if retry.ServerSeq != ack.ServerSeq || retry.RejectionReason != nil {
		t.Fatal("duplicate approval was not acknowledged consistently")
	}
	close(agent.gate)
}

func TestAHPPersistedSessionURI(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "persistence")
	uri := "ahp-session:/durable"
	chat := create(t, client, uri)
	other, _, agent := newProtocolServer(t)
	h := other.Project
	a, ch, err := h.host.ensureSession(chat)
	if err != nil {
		t.Fatal(err)
	}
	if a.resource != uri || ch.controller.ref.SessionID != "session-1" || agent.loaded.Load() != 1 {
		t.Fatalf("session alias did not recover: %+v", a)
	}
	if s.Project.scope.WorkspaceID != other.Project.scope.WorkspaceID {
		t.Fatal("fixture must share the workspace namespace")
	}
}

// protocolTestGateway answers the builtin agent's model list and one streamed reply.
func protocolTestGateway(t *testing.T) {
	t.Helper()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6-sol"}]}`)
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"answer\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello from Wingman\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"answer\",\"role\":\"assistant\",\"status\":\"completed\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello from Wingman\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-6-sol")
}

func TestAHPBuiltinAgentRoundTrip(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app)
	defer server.Close()
	client := protocolTestClient(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ahp", "builtin")
	var config types.ResolveSessionConfigResult
	if err := client.Request(t.Context(), "resolveSessionConfig", types.ResolveSessionConfigParams{Channel: types.RootResourceURI, Provider: new("wingman")}, &config); err != nil {
		t.Fatal(err)
	}
	if string(config.Values["model"]) != `"gpt-6-sol"` {
		t.Fatalf("model configuration missing: %+v", config)
	}
	uri := "ahp-session:/builtin"
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: uri, Provider: new("wingman"), WorkingDirectories: []string{app.Project.directoryURI()}, Config: config.Values}, nil); err != nil {
		t.Fatal(err)
	}
	state, sub := subscribeChat(t, client, ahp.ChatURI(uri))
	send(t, client, state.Resource, "builtin-turn", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
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
	_, _, _, endpoint := protocolTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "")
	_, err := client.Initialize(ctx, "old", []string{"99.0.0"}, nil)
	if err == nil {
		t.Fatal("incompatible version accepted")
	}
}

func TestAHPVSCodeDesktopHandshake(t *testing.T) {
	_, _, _, endpoint := protocolTestServer(t)
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
		Error  *ahp.Error             `json:"error"`
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
	s, _, _, endpoint := protocolTestServer(t)
	workspace, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Project.workspace = workspace
	s.Project = webProject(s, workspace.RootPath)
	client := protocolTestClient(t, endpoint, "missing-resources")
	for _, method := range []string{"resourceRead", "resourceResolve", "resourceList"} {
		err := client.Request(t.Context(), method, map[string]any{"channel": types.RootResourceURI, "uri": s.Project.directoryURI() + "/missing"}, nil)
		var rpc *sdk.RPCError
		if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeNotFound {
			t.Fatalf("%s: expected NotFound, got %v", method, err)
		}
	}
	_, _, err = client.Subscribe(t.Context(), "ahp-session:/missing")
	var rpc *sdk.RPCError
	if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeSessionNotFound {
		t.Fatalf("expected SessionNotFound, got %v", err)
	}
}

func TestAHPOriginPolicy(t *testing.T) {
	_, _, _, endpoint := protocolTestServer(t)
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

func TestAHPSlowRequestDoesNotBlockTheConnection(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	agent.creating = make(chan struct{})
	client := protocolTestClient(t, endpoint, "slow")
	created := make(chan error, 1)
	go func() {
		created <- client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: "ahp-session:/slow", Provider: new("one")}, nil)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, nil); err != nil {
		t.Fatalf("a request waited for a slow session creation: %v", err)
	}
	close(agent.creating)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}

func TestAHPReconnectUnknownClientCanInitialize(t *testing.T) {
	for _, version := range []string{"1.0.0", "1.1.0"} {
		for _, restarted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restarted=%t", version, restarted), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				const clientID = "recovering-client"
				var lastSeen int64
				if restarted {
					oldServer, _, _, oldEndpoint := protocolTestServer(t)
					oldClient := protocolTestClient(t, oldEndpoint, "")
					result, err := oldClient.Initialize(ctx, clientID, []string{version}, []string{types.RootResourceURI})
					if err != nil {
						t.Fatal(err)
					}
					lastSeen = result.ServerSeq
					_ = oldClient.Shutdown(ctx)
					oldServer.Close()
				}
				_, _, _, endpoint := protocolTestServer(t)
				client := protocolTestClient(t, endpoint, "")
				// VS Code can reconnect before its first successful initialize,
				// or after the host restarts and loses its negotiated versions.
				_, err := client.Reconnect(ctx, clientID, lastSeen, []string{types.RootResourceURI})
				var rpc *sdk.RPCError
				if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeNotFound {
					t.Fatalf("want NotFound to trigger a fresh initialize, got %v", err)
				}
				// Reuse the same transport, as VS Code's automatic fallback does.
				result, err := client.Initialize(ctx, clientID, []string{version}, []string{types.RootResourceURI})
				if err != nil {
					t.Fatalf("fresh initialize after reconnect: %v", err)
				}
				if result.ProtocolVersion != version || len(result.Snapshots) != 1 || result.Snapshots[0].State.Root == nil {
					t.Fatalf("recovery lost protocol negotiation or root state: %+v", result)
				}
				chat := create(t, client, "ahp-session:/after-recovery")
				// The recovered identity also supports normal subsequent reconnects.
				next := protocolTestClient(t, endpoint, "")
				recovered, err := next.Reconnect(ctx, clientID, result.ServerSeq, []string{types.RootResourceURI, chat})
				if err != nil {
					t.Fatal(err)
				}
				snapshot, ok := recovered.Value.(*types.ReconnectSnapshotResult)
				if !ok || len(snapshot.Snapshots) != 2 {
					t.Fatalf("missing recovered subscriptions: %+v", recovered)
				}
			})
		}
	}
}

func TestAHPUnknownSessionLeavesNoController(t *testing.T) {
	s, b, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "probe")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := client.Subscribe(ctx, s.Project.canonicalURI("one", "missing")); err == nil {
		t.Fatal("subscribing to an unknown session succeeded")
	}
	b.mu.Lock()
	_, kept := b.sessions["missing"]
	b.mu.Unlock()
	if kept {
		t.Fatal("an unknown session id left a controller behind")
	}
}
