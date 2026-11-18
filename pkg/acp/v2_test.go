package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	load   func(context.Context, acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error)
	fork   func(context.Context, acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error)
	model  string
}

func (a *versionedAgent) SetClientConnection(conn acp.ClientConnection) { a.conn = conn }
func (a *versionedAgent) Initialize(context.Context, acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	caps := acpsdk.SessionCapabilities{Resume: &acpsdk.SessionResumeCapabilities{}}
	if a.fork != nil {
		caps.Fork = &acpsdk.SessionForkCapabilities{}
	}
	return acpsdk.InitializeResponse{ProtocolVersion: 1, AgentInfo: &acpsdk.Implementation{Name: "test", Version: "1"}, AgentCapabilities: acpsdk.AgentCapabilities{LoadSession: true, SessionCapabilities: caps}}, nil
}
func (a *versionedAgent) UnstableForkSession(ctx context.Context, p acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error) {
	return a.fork(ctx, p)
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
	if a.load != nil {
		return a.load(ctx, p)
	}
	for _, text := range []string{"saved ", "answer"} {
		u := acpsdk.UpdateAgentMessageText(text)
		u.AgentMessageChunk.MessageId = new(acpsdk.MessageId("saved"))
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
		if len(u) != 6 || u[0]["sessionUpdate"] != "agent_message" || len(u[0]["content"].([]any)) != 0 || u[0]["_meta"] != nil || u[3]["sessionUpdate"] != "replay_complete" || u[4]["sessionUpdate"] != "session_info_update" || u[5]["state"] != "idle" {
			t.Fatalf("replay didn't replace then snapshot: %v", u)
		}
	}
}

func TestV2ResumePreservesMetadataAndAcceptsGroupedConfig(t *testing.T) {
	const generation = "9007199254740993"
	a := &versionedAgent{model: "one"}
	a.resume = func(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
		groups := acpsdk.SessionConfigSelectOptionsGrouped{
			{Group: "local", Name: "Local", Options: []acpsdk.SessionConfigSelectOption{{Value: "one", Name: "One"}}},
			{Group: "remote", Name: "Remote", Options: []acpsdk.SessionConfigSelectOption{{Value: "two", Name: "Two"}}},
		}
		options := a.options()
		options[0].Select.Options = acpsdk.SessionConfigSelectOptions{Grouped: &groups}
		return acpsdk.ResumeSessionResponse{
			Meta:          acpsdk.Meta{"example.org/generation": json.RawMessage(generation)},
			ConfigOptions: options,
		}, nil
	}
	p := newV2Peer(t, a)
	p.initialize(t)
	result, err := acpsdk.SendRequest[json.RawMessage](p.rpc, t.Context(), "session/resume", map[string]any{"sessionId": "s", "cwd": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var resumed struct {
		Meta          acpsdk.Meta `json:"_meta"`
		ConfigOptions []struct {
			ConfigId string
			Options  []struct{ GroupId string }
		}
	}
	if err := json.Unmarshal(result, &resumed); err != nil {
		t.Fatal(err)
	}
	if string(resumed.Meta["example.org/generation"]) != generation || len(resumed.ConfigOptions) == 0 {
		t.Fatalf("resume response: %s", result)
	}
	model := resumed.ConfigOptions[0]
	if model.ConfigId != "model" || len(model.Options) != 2 || model.Options[1].GroupId != "remote" {
		t.Fatalf("grouped model options: %s", result)
	}
	params := map[string]any{"sessionId": "s", "configId": "model", "type": "id", "value": "remote"}
	_, err = acpsdk.SendRequest[json.RawMessage](p.rpc, t.Context(), "session/set_config_option", params)
	var rpc *acpsdk.RequestError
	if !errors.As(err, &rpc) || rpc.Code != -32602 {
		t.Fatalf("selecting a group id: %v", err)
	}
	params["value"] = "two"
	selected := p.call(t, "session/set_config_option", params)
	if model := selected["configOptions"].([]any)[0].(map[string]any); model["currentValue"] != "two" {
		t.Fatalf("selected model: %v", model)
	}
}

func TestV2CloseCancelsPendingElicitation(t *testing.T) {
	a := &versionedAgent{model: "one"}
	asked := make(chan struct{})
	finished := make(chan struct{})
	a.prompt = func(ctx context.Context, _ acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "")
		_, err := a.conn.CreateElicitation(ctx, acpsdk.CreateElicitationRequest{Form: &acpsdk.CreateElicitationForm{Mode: "form", Message: "Answer?", Session: &acpsdk.ElicitationSessionScope{SessionId: "s"}, RequestedSchema: acpsdk.ElicitationSchema{Type: acpsdk.ElicitationSchemaTypeObject, Properties: map[string]acpsdk.ElicitationPropertySchema{}}}})
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

func TestV2ForkRejectsUnknownCursorWithoutForking(t *testing.T) {
	var forks atomic.Int32
	a := &versionedAgent{model: "one", fork: func(_ context.Context, p acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error) {
		forks.Add(1)
		return acpsdk.UnstableForkSessionResponse{SessionId: "child"}, nil
	}}
	peer := newV2Peer(t, a)
	peer.initialize(t)
	params := map[string]any{"sessionId": "s", "cwd": t.TempDir()}
	for _, cursor := range []any{"other-session-cursor", "", 17} {
		params["cursorId"] = cursor
		_, err := acpsdk.SendRequest[any](peer.rpc, t.Context(), "session/fork", params)
		var failure *acpsdk.RequestError
		if !errors.As(err, &failure) || failure.Code != -32602 || forks.Load() != 0 {
			t.Fatalf("cursor %v: err %v, forks %d", cursor, err, forks.Load())
		}
	}
	params["cursorId"] = nil
	if res := peer.call(t, "session/fork", params); res["sessionId"] != "child" || forks.Load() != 1 {
		t.Fatalf("whole-session fork = %v", res)
	}
}

func TestV2ReplaySeparatesConcurrentLiveUpdatesAndResetsMetadata(t *testing.T) {
	a := &versionedAgent{model: "one"}
	historyStarted, liveSent := make(chan struct{}), make(chan struct{})
	a.load = func(ctx context.Context, req acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
		close(historyStarted)
		select {
		case <-liveSent:
		case <-ctx.Done():
			return acpsdk.LoadSessionResponse{}, ctx.Err()
		}
		tool := acpsdk.StartToolCall("job", "Old job", acpsdk.WithStartStatus("completed"))
		tool.ToolCall.Meta = acpsdk.Meta{"example.org/label": json.RawMessage(`"retained"`)}
		err := a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: req.SessionId, Update: tool})
		return acpsdk.LoadSessionResponse{}, err
	}
	p := newV2Peer(t, a)
	p.initialize(t)
	ctx := t.Context()
	go func() {
		defer close(liveSent)
		select {
		case <-historyStarted:
		case <-ctx.Done():
			return
		}
		for _, u := range []acpsdk.SessionUpdate{
			{Notice: &acpsdk.SessionUpdateNotice{SessionUpdate: "notice", Title: "Live advisory", Severity: "info"}},
			acpsdk.UpdateAgentMessageText("new output"),
			acpsdk.UpdateToolCall("job", acpsdk.WithUpdateStatus("in_progress"), acpsdk.WithUpdateTitle("late job output")),
		} {
			if err := a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: "s", Update: u}); err != nil {
				t.Errorf("live update: %v", err)
				return
			}
		}
	}()
	p.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": t.TempDir(), "replayFrom": map[string]any{"type": "start"}})
	updates := p.snapshot()
	want := []string{"tool_call_update", "tool_call_update", "replay_complete", "notice", "agent_message", "agent_message_chunk", "tool_call_update", "session_info_update", "state_update"}
	if len(updates) != len(want) {
		t.Fatalf("updates = %v", updates)
	}
	for i, kind := range want {
		if updates[i]["sessionUpdate"] != kind {
			t.Fatalf("update %d = %v, want %s", i, updates[i], kind)
		}
	}
	if meta, ok := updates[0]["_meta"]; !ok || meta != nil || updates[1]["_meta"].(map[string]any)["example.org/label"] != "retained" {
		t.Fatalf("replayed metadata patches = %v", updates[:2])
	}
	if _, ok := updates[6]["status"]; ok || updates[6]["title"] != "late job output" {
		t.Fatalf("live patch reopened replayed job: %v", updates[6])
	}
}

func TestV2ElicitationDropsOnlyStringPattern(t *testing.T) {
	a := &versionedAgent{model: "one"}
	peer := newV2Peer(t, a)
	peer.interaction = func(_ context.Context, _ string, params map[string]any) (any, *acpsdk.RequestError) {
		properties := params["requestedSchema"].(map[string]any)["properties"].(map[string]any)
		field := properties["code"].(map[string]any)
		if _, ok := field["pattern"]; ok || field["minLength"] != 2.0 || field["title"] != "Code" {
			t.Errorf("v2 field = %v", field)
		}
		return map[string]any{"action": "accept", "content": map[string]any{"code": "abc"}}, nil
	}
	peer.initialize(t)
	field := &acpsdk.ElicitationPropertySchemaString{Type: "string", MinLength: new(int64(2)), Title: new("Code"), Pattern: new("^[a-z]+$")}
	form := &acpsdk.CreateElicitationForm{Mode: "form", Message: "Code?", Session: &acpsdk.ElicitationSessionScope{SessionId: "s"},
		RequestedSchema: acpsdk.ElicitationSchema{Type: "object", Properties: map[string]acpsdk.ElicitationPropertySchema{"code": {String: field}}}}
	answer, err := a.conn.CreateElicitation(t.Context(), acpsdk.CreateElicitationRequest{Form: form})
	if err != nil || answer.Accept == nil || field.Pattern == nil || *field.Pattern != "^[a-z]+$" {
		t.Fatalf("answer = %+v, err = %v, original pattern = %v", answer, err, field.Pattern)
	}
}

func TestV2LateToolReportsKeepFinalStatusAndOutput(t *testing.T) {
	a := &versionedAgent{model: "one"}
	peer := newV2Peer(t, a)
	peer.initialize(t)
	clearStatus := acpsdk.UpdateToolCall("job")
	clearStatus.ToolCallUpdate.Status = acpsdk.PatchNull[acpsdk.ToolCallStatus]()
	for _, u := range []acpsdk.SessionUpdate{
		acpsdk.StartToolCall("job", "Job", acpsdk.WithStartStatus("in_progress")),
		clearStatus,
		acpsdk.UpdateToolCall("job", acpsdk.WithUpdateStatus("paused")),
		acpsdk.UpdateToolCall("job", acpsdk.WithUpdateStatus("in_progress")),
		acpsdk.UpdateToolCall("job", acpsdk.WithUpdateStatus("completed")),
		acpsdk.StartToolCall("job", "Late output", acpsdk.WithStartStatus("pending"), acpsdk.WithStartRawOutput("finished")),
		acpsdk.UpdateToolCall("job", acpsdk.WithUpdateStatus("failed")),
	} {
		if err := a.conn.SessionUpdate(t.Context(), acpsdk.SessionNotification{SessionId: "s", Update: u}); err != nil {
			t.Fatal(err)
		}
	}
	// A request/response on this pipe drains prior notifications.
	peer.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": t.TempDir()})
	updates := peer.snapshot()
	for i, want := range []string{"in_progress", "", "paused", "in_progress", "completed", "", ""} {
		status, present := updates[i]["status"]
		if want == "" && present || want != "" && status != want {
			t.Fatalf("update %d = %v", i, updates[i])
		}
	}
	if u := updates[5]; u["rawOutput"] != "finished" || u["title"] != "Late output" {
		t.Fatalf("lost late fields: %v", u)
	} else if _, cleared := u["rawInput"]; cleared {
		t.Fatalf("duplicate start cleared existing fields: %v", u)
	}
}

func TestV2DiffPathsAndMissingSides(t *testing.T) {
	a := &versionedAgent{model: "one"}
	peer := newV2Peer(t, a)
	cwd := t.TempDir()
	peer.initialize(t)
	peer.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": cwd})
	file := "space é.txt"
	add := acpsdk.ToolDiffContent(file, "new")
	del := acpsdk.ToolDiffContent(file, "", "old\n")
	del.Diff.Meta = acpsdk.Meta{"kind": json.RawMessage(`"delete"`)}
	for _, part := range []acpsdk.ToolCallContent{add, del} {
		err := a.conn.SessionUpdate(t.Context(), acpsdk.SessionNotification{SessionId: "s", Update: acpsdk.UpdateToolCall("t", acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{part}))})
		if err != nil {
			t.Fatal(err)
		}
	}
	peer.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": cwd})
	var count int
	for _, update := range peer.snapshot() {
		if update["sessionUpdate"] != "tool_call_update" {
			continue
		}
		diff := update["content"].([]any)[0].(map[string]any)
		change := diff["changes"].([]any)[0].(map[string]any)
		patch := diff["patch"].(map[string]any)["text"].(string)
		operation := []string{"add", "delete"}[count]
		missing := []string{"--- /dev/null\n", "+++ /dev/null\n"}[count]
		if change["operation"] != operation || change["path"] != filepath.Join(cwd, file) || !strings.Contains(patch, missing) ||
			!strings.Contains(patch, `space \303\251.txt"`) || strings.Contains(patch, `"a/`) || strings.Contains(patch, `"b/`) {
			t.Fatalf("invalid %s diff: %v", operation, diff)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("got %d diffs", count)
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
		{"session/set_config_option", map[string]any{"sessionId": "s", "configId": "model", "type": "id", "value": []string{"two"}}},
		{"session/set_config_option", map[string]any{"sessionId": "s", "configId": "model", "type": "boolean", "value": true}},
		{"session/set_config_option", map[string]any{"sessionId": "s", "configId": "model", "value": "two"}},
	} {
		_, err := acpsdk.SendRequest[map[string]any](p.rpc, t.Context(), tt.method, tt.params)
		var rpc *acpsdk.RequestError
		want := -32602
		if tt.params.(map[string]any)["sessionId"] == "missing" {
			want = -32002
		}
		if !errors.As(err, &rpc) || rpc.Code != want {
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
		_, err := a.conn.CreateElicitation(acp.WithBackgroundInteraction(t.Context()), acpsdk.CreateElicitationRequest{Form: &acpsdk.CreateElicitationForm{
			Mode: "form", Message: "Background question", Session: &acpsdk.ElicitationSessionScope{SessionId: "s"},
			RequestedSchema: acpsdk.ElicitationSchema{Type: acpsdk.ElicitationSchemaTypeObject, Properties: map[string]acpsdk.ElicitationPropertySchema{}},
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
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32002 {
		t.Fatalf("prompt after close = %v", err)
	}
}

func TestV2RejectsInvalidFramesWithoutDispatch(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	client.SetDeadline(time.Now().Add(5 * time.Second))
	a := &versionedAgent{model: "one", prompt: func(context.Context, acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		t.Error("invalid frame reached backend")
		return acpsdk.PromptResponse{}, nil
	}}
	acp.NewAgentConnection(a, server, server)
	dec := json.NewDecoder(client)
	exchange := func(line string) map[string]any {
		t.Helper()
		if _, err := io.WriteString(client, line+"\n"); err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := dec.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"test","version":"1"}}}`)
	for _, line := range []string{`[]`, `[{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/"}}]`, `{"jsonrpc":"2.0","id":null,"method":"session/new","params":{"cwd":"/"}}`, `{"jsonrpc":"2.0","id":1.25,"method":"session/new","params":{"cwd":"/"}}`} {
		frame := exchange(line)
		e, _ := frame["error"].(map[string]any)
		if id, present := frame["id"]; !present || id != nil || e["code"] != float64(-32600) {
			t.Fatalf("%s: %v", line, frame)
		}
	}
	if frame := exchange(`{"jsonrpc":"2.0","id":"ok","method":"session/new","params":{"cwd":"/"}}`); frame["result"] == nil {
		t.Fatal(frame)
	}
}

// A retry before insertion joins the pending submission. A retry after
// insertion, including on a fresh connection, returns only the recorded ID.
func TestV2KeyedPromptAcrossConcurrentRequestsAndReconnect(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	begun := make(chan struct{})
	insert := make(chan struct{})
	finish := make(chan struct{})
	var executions atomic.Int32
	a := &versionedAgent{model: "one", prompt: func(ctx context.Context, _ acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		if executions.Add(1) != 1 {
			return acpsdk.PromptResponse{}, errors.New("duplicate execution")
		}
		close(begun)
		select {
		case <-insert:
		case <-ctx.Done():
			return acpsdk.PromptResponse{}, ctx.Err()
		}
		acp.PromptAccepted(ctx, "recorded")
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}}
	p := newV2Peer(t, a)
	p.initialize(t)
	params := map[string]any{"sessionId": "s", "idempotencyKey": "private-key", "prompt": []any{map[string]any{"type": "text", "text": "run"}}}
	results := make(chan string, 2)
	call := func() {
		r, err := acpsdk.SendRequest[map[string]any](p.rpc, ctx, "session/prompt", params)
		if err != nil {
			results <- err.Error()
		} else {
			results <- fmt.Sprint(r["messageId"])
		}
	}
	go call()
	select {
	case <-begun:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go call()
	close(insert)
	for range 2 {
		select {
		case id := <-results:
			if id != "recorded" {
				t.Fatal(id)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(finish)
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
	cold := newV2Peer(t, &versionedAgent{model: "one", prompt: func(context.Context, acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		return acpsdk.PromptResponse{}, errors.New("re-executed stored key")
	}})
	cold.call(t, "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "test", "version": "1"}})
	cold.call(t, "session/resume", map[string]any{"sessionId": "s", "cwd": t.TempDir()})
	before := len(cold.snapshot())
	r := cold.call(t, "session/prompt", params)
	if r["messageId"] != "recorded" || len(cold.snapshot()) != before {
		t.Fatalf("retry = %v, updates = %v", r, cold.snapshot()[before:])
	}
}

func TestV2RejectedSubmissionReleasesKey(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	var calls atomic.Int32
	a := &versionedAgent{model: "one", prompt: func(ctx context.Context, _ acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		if calls.Add(1) == 1 {
			return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams("rejected before insertion")
		}
		acp.PromptAccepted(ctx, "retried")
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}}
	p := newV2Peer(t, a)
	p.initialize(t)
	params := map[string]any{"sessionId": "s", "idempotencyKey": "same", "prompt": []any{map[string]any{"type": "text", "text": "run"}}}
	_, err := acpsdk.SendRequest[map[string]any](p.rpc, t.Context(), "session/prompt", params)
	var rpc *acpsdk.RequestError
	if !errors.As(err, &rpc) || rpc.Code != -32602 {
		t.Fatalf("first submission = %v", err)
	}
	if got := p.call(t, "session/prompt", params)["messageId"]; got != "retried" {
		t.Fatalf("retry = %v", got)
	}
	p.call(t, "session/close", map[string]any{"sessionId": "s"})
}

func TestV2CancelWithdrawsHeldPromptsAndTheirRetries(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(6 * time.Second))
	var calls atomic.Int32
	agent := &versionedAgent{model: "one", prompt: func(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		calls.Add(1)
		text := p.Prompt[0].Text.Text
		acp.PromptAccepted(ctx, text)
		if text == "active" {
			<-ctx.Done()
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}}
	acp.NewAgentConnection(agent, server, server)
	enc, dec := json.NewEncoder(client), json.NewDecoder(client)
	send := func(id, method string, params any) {
		t.Helper()
		frame := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id != "" {
			frame["id"] = id
		}
		if err := enc.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	response := func() (string, map[string]any) {
		t.Helper()
		for {
			var frame map[string]any
			if err := dec.Decode(&frame); err != nil {
				t.Fatal(err)
			}
			if id, ok := frame["id"].(string); ok {
				return id, frame
			}
		}
	}
	send("init", "initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "test", "version": "1"}})
	response()
	send("new", "session/new", map[string]any{"cwd": t.TempDir()})
	response()
	prompt := func(id, key, text string) {
		params := map[string]any{"sessionId": "s", "prompt": []any{acpsdk.TextBlock(text)}}
		if key != "" {
			params["idempotencyKey"] = key
		}
		send(id, "session/prompt", params)
	}
	prompt("one", "", "active")
	response()
	prompt("two", "retry-me", "held")
	prompt("three", "retry-me", "held")
	send("", "session/cancel", map[string]any{"sessionId": "s"})
	seen := map[string]bool{}
	for range 2 {
		id, frame := response()
		failure, _ := frame["error"].(map[string]any)
		if failure["code"] != -32800.0 {
			t.Fatalf("held prompt %s = %v", id, frame)
		}
		seen[id] = true
	}
	if !seen["two"] || !seen["three"] || calls.Load() != 1 {
		t.Fatalf("cancelled requests %v, backend calls %d", seen, calls.Load())
	}
	prompt("four", "retry-me", "held")
	_, reply := response()
	result, _ := reply["result"].(map[string]any)
	if result["messageId"] != "held" || calls.Load() != 2 {
		t.Fatalf("retry = %v, backend calls %d", reply, calls.Load())
	}
}

func TestV2OpaqueToolDataKeepsLargeIntegers(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	agent := &versionedAgent{model: "one"}
	agent.prompt = func(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
		acp.PromptAccepted(ctx, "inserted")
		update := acpsdk.StartToolCall("tool", "Lookup", acpsdk.WithStartRawInput(json.RawMessage(`{"id":9007199254740993,"_meta":{"literal":true}}`)))
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, agent.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: p.SessionId, Update: update})
	}
	acp.NewAgentConnection(agent, server, server)
	enc, dec := json.NewEncoder(client), json.NewDecoder(client)
	dec.UseNumber()
	call := func(id, method string, params any) {
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
	}
	next := func() map[string]any {
		var f map[string]any
		if err := dec.Decode(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	call("init", "initialize", map[string]any{"protocolVersion": 2, "info": map[string]string{"name": "test", "version": "1"}})
	next()
	call("new", "session/new", map[string]any{"cwd": t.TempDir()})
	next()
	call("prompt", "session/prompt", map[string]any{"sessionId": "s", "prompt": []any{acpsdk.TextBlock("test")}})
	for {
		f := next()
		params, _ := f["params"].(map[string]any)
		u, _ := params["update"].(map[string]any)
		if u["sessionUpdate"] == "tool_call_update" {
			got, _ := u["rawInput"].(map[string]any)
			meta, _ := got["_meta"].(map[string]any)
			if got["id"] != json.Number("9007199254740993") || meta["literal"] != true {
				t.Fatalf("raw input = %#v", got)
			}
			return
		}
	}
}
