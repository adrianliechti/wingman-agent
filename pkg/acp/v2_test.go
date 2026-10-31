package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

type versionedAgent struct {
	acpsdk.Agent
	conn   acp.ClientConnection
	prompt func(context.Context, acpsdk.PromptRequest) (acpsdk.PromptResponse, error)
	resume func(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error)
	model  string
}

func (a *versionedAgent) SetClientConnection(conn acp.ClientConnection) { a.conn = conn }
func (a *versionedAgent) Initialize(context.Context, acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{ProtocolVersion: 1, AgentInfo: &acpsdk.Implementation{Name: "test", Version: "1"}, AgentCapabilities: acpsdk.AgentCapabilities{LoadSession: true}}, nil
}
func (a *versionedAgent) options() []acpsdk.SessionConfigOption {
	values := acpsdk.SessionConfigSelectOptionsUngrouped{{Value: "one", Name: "One"}, {Value: "two", Name: "Two"}}
	option := acpsdk.NewSessionConfigOptionSelect(acpsdk.SessionConfigValueId(a.model), acpsdk.SessionConfigSelectOptions{Ungrouped: &values})
	option.Select.Id = "model"
	option.Select.Name = "Model"
	return []acpsdk.SessionConfigOption{option}
}
func (a *versionedAgent) NewSession(context.Context, acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	return acpsdk.NewSessionResponse{SessionId: "s", ConfigOptions: a.options(), Modes: &acpsdk.SessionModeState{CurrentModeId: "agent", AvailableModes: []acpsdk.SessionMode{{Id: "agent", Name: "Agent"}, {Id: "plan", Name: "Plan"}}}}, nil
}
func (a *versionedAgent) ResumeSession(ctx context.Context, p acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	if a.resume != nil {
		return a.resume(ctx, p)
	}
	return acpsdk.ResumeSessionResponse{ConfigOptions: a.options()}, nil
}
func (a *versionedAgent) LoadSession(ctx context.Context, p acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	for _, text := range []string{"saved ", "answer"} {
		u := acpsdk.UpdateAgentMessageText(text)
		u.AgentMessageChunk.MessageId = new("saved")
		if err := a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: p.SessionId, Update: u}); err != nil {
			return acpsdk.LoadSessionResponse{}, err
		}
	}
	return acpsdk.LoadSessionResponse{ConfigOptions: a.options()}, nil
}
func (a *versionedAgent) SetSessionConfigOption(ctx context.Context, p acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	before := a.options()
	a.model = string(p.ValueId.Value)
	after := a.options()
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: after}, acp.NotifyConfig(ctx, a.conn, "s", before, after)
}
func (a *versionedAgent) SetSessionMode(context.Context, acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	return acpsdk.SetSessionModeResponse{}, nil
}
func (a *versionedAgent) Prompt(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	return a.prompt(ctx, p)
}
func (a *versionedAgent) Cancel(context.Context, acpsdk.CancelNotification) error { return nil }
func (a *versionedAgent) CloseSession(context.Context, acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	return acpsdk.CloseSessionResponse{}, nil
}

type v2Peer struct {
	rpc         *acpsdk.Connection
	mu          sync.Mutex
	updates     []map[string]any
	interaction func(context.Context, string, map[string]any) (any, *acpsdk.RequestError)
}

func newV2Peer(t *testing.T, agent *versionedAgent) *v2Peer {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	acp.NewAgentConnection(agent, server, server)
	p := &v2Peer{}
	p.rpc = acpsdk.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acpsdk.RequestError) {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		if method == "session/update" {
			acptest.ValidateV2(t, "UpdateSessionNotification", params)
			p.mu.Lock()
			p.updates = append(p.updates, params["update"].(map[string]any))
			p.mu.Unlock()
			return nil, nil
		}
		if method == "session/request_permission" {
			acptest.ValidateV2(t, "RequestPermissionRequest", params)
		}
		if method == "elicitation/create" {
			acptest.ValidateV2(t, "CreateElicitationRequest", params)
		}
		if p.interaction != nil {
			return p.interaction(ctx, method, params)
		}
		return nil, acpsdk.NewMethodNotFound(method)
	}, client, client)
	return p
}

func (p *v2Peer) call(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := acpsdk.SendRequest[map[string]any](p.rpc, ctx, method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	names := map[string]string{"initialize": "InitializeResponse", "session/new": "NewSessionResponse", "session/resume": "ResumeSessionResponse", "session/prompt": "PromptResponse", "session/set_config_option": "SetSessionConfigOptionResponse"}
	if name := names[method]; name != "" {
		acptest.ValidateV2(t, name, result)
	}
	return result
}
func (p *v2Peer) snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.updates...)
}
func (p *v2Peer) initialize(t *testing.T) {
	p.call(t, "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"auth": map[string]any{"terminal": map[string]any{}}, "elicitation": map[string]any{"form": map[string]any{}}}})
	p.call(t, "session/new", map[string]any{"cwd": t.TempDir()})
}

func TestV2PromptAcknowledgesAcceptanceBeforeCompletion(t *testing.T) {
	a := &versionedAgent{model: "one"}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	a.prompt = func(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "")
		select {
		case <-release:
		case <-ctx.Done():
			return acpsdk.PromptResponse{}, ctx.Err()
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}
	p := newV2Peer(t, a)
	p.initialize(t)
	response := p.call(t, "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "text", "text": "hello"}}})
	if response["messageId"] == nil {
		t.Fatal("missing accepted message id")
	}
	updates := p.snapshot()
	if len(updates) != 2 || updates[0]["sessionUpdate"] != "user_message" || updates[1]["state"] != "running" {
		t.Fatalf("updates at acceptance: %v", updates)
	}
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
	closed := p.snapshot()
	if len(closed) != len(updates)+1 || closed[len(closed)-1]["state"] != "idle" || closed[len(closed)-1]["stopReason"] != "cancelled" {
		t.Fatalf("close must settle the turn before responding: %v", closed)
	}
}

func TestV2ConfigNotificationsAndReplayReplaceState(t *testing.T) {
	a := &versionedAgent{model: "one"}
	p := newV2Peer(t, a)
	p.initialize(t)
	p.call(t, "session/set_config_option", map[string]any{"sessionId": "s", "configId": "model", "type": "id", "value": "two"})
	updates := p.snapshot()
	if len(updates) != 1 || updates[0]["sessionUpdate"] != "config_option_update" {
		t.Fatalf("configuration notifications: %v", updates)
	}
	p.call(t, "session/set_config_option", map[string]any{"sessionId": "s", "configId": "mode", "type": "id", "value": "plan"})
	updates = p.snapshot()
	options := updates[len(updates)-1]["configOptions"].([]any)
	if len(options) != 2 || options[1].(map[string]any)["currentValue"] != "plan" {
		t.Fatalf("mode options: %v", options)
	}
	for range 2 {
		before := len(p.snapshot())
		p.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": t.TempDir(), "replayFrom": map[string]any{"type": "start"}})
		u := p.snapshot()[before:]
		if len(u) != 4 || u[0]["sessionUpdate"] != "agent_message" || len(u[0]["content"].([]any)) != 0 || u[0]["_meta"] != nil || u[3]["state"] != "idle" {
			t.Fatalf("replay didn't replace then snapshot: %v", u)
		}
	}
}

func TestV2CloseCancelsPendingElicitation(t *testing.T) {
	a := &versionedAgent{model: "one"}
	asked := make(chan struct{})
	finished := make(chan struct{})
	a.prompt = func(ctx context.Context, _ acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "")
		_, err := a.conn.UnstableCreateElicitation(ctx, acpsdk.UnstableCreateElicitationRequest{Form: &acpsdk.UnstableCreateElicitationForm{Mode: "form", Message: "Answer?", Meta: map[string]any{"sessionId": "s"}, RequestedSchema: acpsdk.UnstableElicitationSchema{Type: acpsdk.UnstableElicitationSchemaTypeObject, Properties: map[string]any{}}}})
		close(finished)
		return acpsdk.PromptResponse{}, err
	}
	p := newV2Peer(t, a)
	p.interaction = func(ctx context.Context, method string, _ map[string]any) (any, *acpsdk.RequestError) {
		if method != "elicitation/create" {
			t.Errorf("unexpected request %s", method)
		}
		close(asked)
		<-ctx.Done()
		return nil, &acpsdk.RequestError{Code: -32800, Message: "cancelled"}
	}
	p.initialize(t)
	p.call(t, "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "text", "text": "ask"}}})
	select {
	case <-asked:
	case <-time.After(3 * time.Second):
		t.Fatal("no elicitation")
	}
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
	select {
	case <-finished:
	default:
		t.Fatal("close returned before the interaction settled")
	}
}

func TestV2RejectsUnsupportedInputBeforeStartingWork(t *testing.T) {
	a := &versionedAgent{model: "one", prompt: func(context.Context, acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		t.Error("invalid prompt reached backend")
		return acpsdk.PromptResponse{}, nil
	}}
	p := newV2Peer(t, a)
	p.initialize(t)
	for _, tt := range []struct {
		method string
		params any
	}{
		{"session/prompt", map[string]any{"sessionId": "s", "prompt": []any{}}},
		{"session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "audio", "data": "AA==", "mimeType": "audio/wav"}}}},
		{"session/prompt", map[string]any{"sessionId": "missing", "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}},
		{"session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{map[string]any{"type": "sse", "name": "unsupported", "url": "https://example.invalid"}}}},
		{"session/set_config_option", map[string]any{"sessionId": "s", "configId": "model", "type": "id", "value": "unknown"}},
	} {
		_, err := acpsdk.SendRequest[map[string]any](p.rpc, t.Context(), tt.method, tt.params)
		var rpc *acpsdk.RequestError
		if !errors.As(err, &rpc) || rpc.Code != -32602 {
			t.Fatalf("%s error = %v", tt.method, err)
		}
	}
	if len(p.snapshot()) != 0 {
		t.Fatal("rejected inputs changed session state")
	}
}

func TestACPVersionsNegotiateOnIndependentConnections(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version any
		want    float64
		code    int
	}{
		{"v1", 1, 1, 0},
		{"v2", 2, 2, 0},
		{"future", 65535, 2, 0},
		{"unsupported", 0, 0, -32022},
		{"missing", nil, 0, -32602},
		{"negative", -1, 0, -32602},
		{"fraction", 1.5, 0, -32602},
		{"overflow", 65536, 0, -32602},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			peer := newV2Peer(t, &versionedAgent{})
			params := map[string]any{"info": map[string]any{"name": "test", "version": "1"}}
			if tt.version != nil {
				params["protocolVersion"] = tt.version
			}
			result, err := acpsdk.SendRequest[map[string]any](peer.rpc, t.Context(), "initialize", params)
			if tt.code != 0 {
				var rpc *acpsdk.RequestError
				if !errors.As(err, &rpc) || rpc.Code != tt.code {
					t.Fatalf("initialize = %v, %v", result, err)
				}
				return
			}
			if err != nil || result["protocolVersion"] != tt.want {
				t.Fatalf("initialize = %v, %v", result, err)
			}
		})
	}
}

func TestV2CloseCancelsBackgroundElicitation(t *testing.T) {
	a := &versionedAgent{model: "one"}
	p := newV2Peer(t, a)
	asked := make(chan struct{})
	p.interaction = func(ctx context.Context, _ string, _ map[string]any) (any, *acpsdk.RequestError) {
		close(asked)
		<-ctx.Done()
		return nil, &acpsdk.RequestError{Code: -32800, Message: "cancelled"}
	}
	p.initialize(t)
	finished := make(chan error, 1)
	go func() {
		_, err := a.conn.UnstableCreateElicitation(t.Context(), acpsdk.UnstableCreateElicitationRequest{Form: &acpsdk.UnstableCreateElicitationForm{
			Mode: "form", Message: "Background question", Meta: map[string]any{"sessionId": "s", "codex": map[string]any{"delivery": "async"}},
			RequestedSchema: acpsdk.UnstableElicitationSchema{Type: acpsdk.UnstableElicitationSchemaTypeObject, Properties: map[string]any{}},
		}})
		finished <- err
	}()
	select {
	case <-asked:
	case <-time.After(3 * time.Second):
		t.Fatal("background form did not open")
	}
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
	select {
	case err := <-finished:
		var rpc *acpsdk.RequestError
		if !errors.Is(err, context.Canceled) && !(errors.As(err, &rpc) && rpc.Code == -32800) {
			t.Fatalf("closed interaction = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("background form outlived its session")
	}
	if len(p.snapshot()) != 0 {
		t.Fatal("asynchronous form changed the foreground state")
	}
}

func TestV2CloseDuringResumeDoesNotReopenSession(t *testing.T) {
	started := make(chan struct{})
	agent := &versionedAgent{model: "one", resume: func(ctx context.Context, _ acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
		close(started)
		<-ctx.Done()
		// Even a backend that finishes successfully after cancellation must not
		// resurrect the closed session at the wire boundary.
		return acpsdk.ResumeSessionResponse{}, nil
	}}
	p := newV2Peer(t, agent)
	p.initialize(t)
	p.call(t, "session/new", map[string]any{"cwd": t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	cwd := t.TempDir()
	go func() {
		_, err := acpsdk.SendRequest[map[string]any](p.rpc, ctx, "session/resume", map[string]any{"sessionId": "s", "cwd": cwd})
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
	var rpcErr *acpsdk.RequestError
	if err := <-finished; !errors.As(err, &rpcErr) || rpcErr.Code != -32800 {
		t.Fatalf("resume during close = %v", err)
	}
	_, err := acpsdk.SendRequest[map[string]any](p.rpc, ctx, "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{map[string]any{"type": "text", "text": "hello"}}})
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("prompt after close = %v", err)
	}
}

type batchedAgent struct {
	*versionedAgent
	closed chan struct{}
}

func (a *batchedAgent) ListSessions(ctx context.Context, _ acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	select {
	case <-a.closed:
		return acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}, nil
	case <-ctx.Done():
		return acpsdk.ListSessionsResponse{}, ctx.Err()
	}
}

func (a *batchedAgent) CloseSession(context.Context, acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	close(a.closed)
	return acpsdk.CloseSessionResponse{}, nil
}

func TestV2CloseSettlesPromptWithBufferedBatchResponse(t *testing.T) {
	agent := &batchedAgent{versionedAgent: &versionedAgent{model: "one"}, closed: make(chan struct{})}
	agent.prompt = func(ctx context.Context, _ acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "accepted")
		<-ctx.Done()
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	acp.NewAgentConnection(agent, server, server)
	encoder, decoder := json.NewEncoder(client), json.NewDecoder(client)
	read := func() any {
		t.Helper()
		var frame any
		if err := decoder.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	write := func(raw string) {
		t.Helper()
		if err := encoder.Encode(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"test","version":"1"},"capabilities":{}}}`)
	read()
	params, _ := json.Marshal(map[string]any{"cwd": t.TempDir()})
	write(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":` + string(params) + `}`)
	read()
	write(`[{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"wait"}]}},{"jsonrpc":"2.0","id":4,"method":"session/list","params":{}}]`)
	// Acceptance emits user_message before the response. The response is held
	// in a batch whose other request is waiting for session/close to finish.
	read()
	write(`{"jsonrpc":"2.0","id":5,"method":"session/close","params":{"sessionId":"s"}}`)
	batch, closed := false, false
	for !batch || !closed {
		switch frame := read().(type) {
		case []any:
			batch = len(frame) == 2
		case map[string]any:
			closed = closed || frame["id"] == float64(5) && frame["error"] == nil
		}
	}
}
