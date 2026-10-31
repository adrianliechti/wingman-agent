package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// These wire-level contracts follow the upstream subscription specification and
// selected VS Code conformance scenarios. They are not the full VS Code suite:
// https://github.com/microsoft/agent-host-protocol/blob/d7a5579b1733fb2e1e4a2c653442e26c7b7226dd/docs/specification/subscriptions.md
// https://github.com/microsoft/vscode/blob/c198526d8e64ba6f9944e9f7bcc079ed1f6d7fcc/src/vs/platform/agentHost/test/node/e2e/suites/protocolContractsSuite.ts
// Only provider discovery/session creation is faked. Assertions use the public
// WebSocket endpoint, without inspecting host state or using browser adapters.
func TestAHPConformance(t *testing.T) {
	for _, version := range []string{"1.0.0", "1.1.0"} {
		t.Run(version, func(t *testing.T) {
			t.Run("subscriptions", func(t *testing.T) { testAHPSubscriptionConformance(t, version) })
			t.Run("rejection_and_reconnect", func(t *testing.T) { testAHPRecoveryConformance(t, version) })
		})
	}
}

func testAHPSubscriptionConformance(t *testing.T, version string) {
	_, _, _, endpoint := ahpTestServer(t)
	first := newAHPConformancePeer(t, endpoint, "first")
	first.initialize(version)
	chat := first.createSession("ahp-session:/conformance")
	otherChat := first.createSession("ahp-session:/other")
	initial := first.subscribe(chat)
	second := newAHPConformancePeer(t, endpoint, "second")
	second.initialize(version, chat)
	// A repeated subscribe replaces the subscription rather than multiplying it.
	second.subscribe(chat)
	second.subscribe(chat)
	outsider := newAHPConformancePeer(t, endpoint, "other-channel")
	outsider.initialize(version, otherChat)

	shared := first.draft(chat, "shared draft")
	if shared.ServerSeq <= initial.FromSeq {
		t.Fatalf("action sequence %d does not follow snapshot %d", shared.ServerSeq, initial.FromSeq)
	}
	first.expectActions(shared)
	second.expectActions(shared)
	outsider.expectActions()
	// Microsoft's reducer must produce the same draft as a fresh host snapshot.
	state := initial.State.Chat
	if state == nil {
		t.Fatal("chat subscription did not return chat state")
	}
	ahp.ApplyActionToChat(state, shared.Action)
	if snapshot := second.subscribe(chat); !reflect.DeepEqual(state, snapshot.State.Chat) {
		t.Fatalf("reduced state differs from snapshot: %+v / %+v", state, snapshot.State.Chat)
	}

	second.notify("unsubscribe", types.UnsubscribeParams{Channel: chat})
	// Dispatch on the same channel after unsubscribe. Its acknowledgement is a
	// processing barrier; a ping alone can overtake an asynchronous unsubscribe.
	barrier := second.draft(chat, "unsubscribe processed")
	second.expectActions(barrier)
	first.expectActions(barrier)
	missed := first.draft(chat, "changed while unsubscribed")
	first.expectActions(missed)
	second.expectActions()
	outsider.expectActions()
	resumed := second.subscribe(chat)
	if resumed.FromSeq < missed.ServerSeq || resumed.State.Chat == nil || resumed.State.Chat.Draft == nil || resumed.State.Chat.Draft.Text != "changed while unsubscribed" {
		t.Fatalf("resubscribe did not recover missed state: %+v", resumed)
	}

	live := first.draft(chat, "live again")
	if live.ServerSeq <= resumed.FromSeq {
		t.Fatalf("live action %d predates resumed snapshot %d", live.ServerSeq, resumed.FromSeq)
	}
	first.expectActions(live)
	second.expectActions(live)
	outsider.expectActions()
}

func testAHPRecoveryConformance(t *testing.T, version string) {
	_, _, _, endpoint := ahpTestServer(t)
	first := newAHPConformancePeer(t, endpoint, "first")
	first.initialize(version)
	chat := first.createSession("ahp-session:/recovery")
	first.subscribe(chat)
	second := newAHPConformancePeer(t, endpoint, "second")
	second.initialize(version, chat)
	accepted := first.draft(chat, "before disconnect")
	first.expectActions(accepted)
	second.expectActions(accepted)

	// A client must not be able to forge server-owned chat activity. Rejection
	// still carries its origin so optimistic clients can reconcile the action.
	before := second.subscribe(chat)
	forged := types.StateAction{Value: &types.ChatActivityChangedAction{Type: types.ActionTypeChatActivityChanged, Activity: ahpPtr("forged activity")}}
	rejected := second.dispatch(chat, forged)
	if rejected.RejectionReason == nil || *rejected.RejectionReason == "" {
		t.Fatalf("server-only action was accepted: %+v", rejected)
	}
	if after := second.subscribe(chat); !reflect.DeepEqual(before.State.Chat, after.State.Chat) {
		t.Fatalf("rejected action changed state: %+v / %+v", before.State.Chat, after.State.Chat)
	}
	// A rejection must not poison later requests on the same connection.
	second.call("ping", map[string]string{"channel": types.RootResourceURI}, nil)
	_ = second.conn.CloseNow()
	missed := first.draft(chat, "changed while disconnected")

	reconnected := newAHPConformancePeer(t, endpoint, "second")
	reconnected.clientSeq = second.clientSeq
	var result types.ReconnectResult
	reconnected.call("reconnect", types.ReconnectParams{Channel: types.RootResourceURI, ClientId: "second", LastSeenServerSeq: rejected.ServerSeq, Subscriptions: []string{chat}}, &result)
	var recovered types.ChatState
	var fromSeq int64
	switch recovery := result.Value.(type) {
	case *types.ReconnectSnapshotResult:
		if len(recovery.Snapshots) != 1 || recovery.Snapshots[0].Resource != chat || recovery.Snapshots[0].State.Chat == nil {
			t.Fatalf("reconnect did not restore the requested chat: %+v", recovery)
		}
		recovered = *recovery.Snapshots[0].State.Chat
		fromSeq = recovery.Snapshots[0].FromSeq
		if fromSeq < missed.ServerSeq {
			t.Fatalf("reconnect snapshot %d predates missed action %d", fromSeq, missed.ServerSeq)
		}
	case *types.ReconnectReplayResult:
		// Replay and fresh snapshots are both permitted by AHP.
		recovered = *before.State.Chat
		fromSeq = rejected.ServerSeq
		for _, action := range recovery.Actions {
			if action.Channel != chat || action.ServerSeq <= fromSeq {
				t.Fatalf("out-of-order or unrequested replay: %+v", action)
			}
			fromSeq = action.ServerSeq
			if action.RejectionReason == nil {
				ahp.ApplyActionToChat(&recovered, action.Action)
			}
		}
	default:
		t.Fatalf("unknown reconnect result: %+v", result)
	}
	if recovered.Draft == nil || recovered.Draft.Text != "changed while disconnected" {
		t.Fatalf("reconnect lost the missed draft: %+v", recovered)
	}
	live := first.draft(chat, "after reconnect")
	if live.ServerSeq <= fromSeq {
		t.Fatalf("live action %d predates recovery %d", live.ServerSeq, fromSeq)
	}
	reconnected.expectActions(live)
	ahp.ApplyActionToChat(&recovered, live.Action)
	if snapshot := reconnected.subscribe(chat); !reflect.DeepEqual(&recovered, snapshot.State.Chat) {
		t.Fatalf("recovered reducer state differs from host snapshot: %+v / %+v", recovered, snapshot.State.Chat)
	}
}

// Read raw frames so duplicate or unsolicited deliveries cannot be hidden by
// an SDK's subscription routing. Protocol payloads use the pinned upstream types.
type ahpConformancePeer struct {
	t         *testing.T
	ctx       context.Context
	conn      *websocket.Conn
	id        string
	requestID int
	clientSeq int64
	actions   []types.ActionEnvelope
}

func newAHPConformancePeer(t *testing.T, endpoint, id string) *ahpConformancePeer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &ahpConformancePeer{t: t, ctx: ctx, conn: conn, id: id}
}

func (p *ahpConformancePeer) initialize(version string, channels ...string) {
	p.t.Helper()
	var result types.InitializeResult
	p.call("initialize", types.InitializeParams{Channel: types.RootResourceURI, ClientId: p.id, ProtocolVersions: []string{"99.0.0", version}, InitialSubscriptions: channels}, &result)
	if result.ProtocolVersion != version || result.ServerSeq < 0 || len(result.Snapshots) != len(channels) {
		p.t.Fatalf("invalid handshake: %+v", result)
	}
	for i, snapshot := range result.Snapshots {
		if snapshot.Resource != channels[i] || snapshot.FromSeq > result.ServerSeq {
			p.t.Fatalf("invalid initial snapshot: %+v", snapshot)
		}
	}
}

func (p *ahpConformancePeer) createSession(uri string) string {
	p.t.Helper()
	p.call("createSession", types.CreateSessionParams{Channel: uri, Provider: ahpPtr("one")}, nil)
	snapshot := p.subscribe(uri)
	if snapshot.State.Session == nil || snapshot.State.Session.DefaultChat == nil {
		p.t.Fatalf("session has no default chat: %+v", snapshot)
	}
	p.notify("unsubscribe", types.UnsubscribeParams{Channel: uri})
	return *snapshot.State.Session.DefaultChat
}

func (p *ahpConformancePeer) subscribe(channel string) types.Snapshot {
	p.t.Helper()
	var result types.SubscribeResult
	p.call("subscribe", types.SubscribeParams{Channel: channel}, &result)
	if result.Snapshot == nil || result.Snapshot.Resource != channel || result.Snapshot.FromSeq < 0 {
		p.t.Fatalf("invalid subscription snapshot: %+v", result)
	}
	return *result.Snapshot
}

func (p *ahpConformancePeer) draft(channel, text string) types.ActionEnvelope {
	p.t.Helper()
	action := types.StateAction{Value: &types.ChatDraftChangedAction{Type: types.ActionTypeChatDraftChanged, Draft: &types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
	envelope := p.dispatch(channel, action)
	if envelope.RejectionReason != nil {
		p.t.Fatalf("draft rejected: %s", *envelope.RejectionReason)
	}
	return envelope
}

func (p *ahpConformancePeer) dispatch(channel string, action types.StateAction) types.ActionEnvelope {
	p.t.Helper()
	p.clientSeq++
	p.notify("dispatchAction", types.DispatchActionParams{Channel: channel, ClientSeq: p.clientSeq, Action: action})
	for {
		p.read()
		if len(p.actions) == 0 {
			continue
		}
		envelope := p.actions[len(p.actions)-1]
		if envelope.Origin != nil && envelope.Origin.ClientId == p.id && envelope.Origin.ClientSeq == p.clientSeq {
			if envelope.Channel != channel || !reflect.DeepEqual(envelope.Action, action) {
				p.t.Fatalf("acknowledgement changed the dispatched action: %+v", envelope)
			}
			return envelope
		}
	}
}

func (p *ahpConformancePeer) expectActions(expected ...types.ActionEnvelope) {
	p.t.Helper()
	// A root snapshot serializes with publication without resubscribing to the
	// chat under test. Ping can overtake a broadcast still reaching other peers.
	p.subscribe(types.RootResourceURI)
	if !reflect.DeepEqual(p.actions, expected) {
		p.t.Fatalf("%s received unexpected actions:\n got %+v\nwant %+v", p.id, p.actions, expected)
	}
	p.actions = nil
}

func (p *ahpConformancePeer) notify(method string, params any) {
	p.t.Helper()
	p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *ahpConformancePeer) call(method string, params, result any) {
	p.t.Helper()
	p.requestID++
	p.write(map[string]any{"jsonrpc": "2.0", "id": p.requestID, "method": method, "params": params})
	for {
		frame := p.read()
		if len(frame["id"]) == 0 {
			continue
		}
		var id int
		if err := json.Unmarshal(frame["id"], &id); err != nil || id != p.requestID || len(frame["error"]) > 0 || len(frame["result"]) == 0 {
			p.t.Fatalf("%s failed: %s", method, frame)
		}
		if result != nil {
			if err := json.Unmarshal(frame["result"], result); err != nil {
				p.t.Fatalf("%s result: %v", method, err)
			}
		}
		return
	}
}

func (p *ahpConformancePeer) write(value any) {
	p.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.conn.Write(p.ctx, websocket.MessageText, data); err != nil {
		p.t.Fatal(err)
	}
}

func (p *ahpConformancePeer) read() map[string]json.RawMessage {
	p.t.Helper()
	kind, data, err := p.conn.Read(p.ctx)
	if err != nil {
		p.t.Fatalf("%s reading AHP frame: %v", p.id, err)
	}
	var frame map[string]json.RawMessage
	if kind != websocket.MessageText || json.Unmarshal(data, &frame) != nil || string(frame["jsonrpc"]) != `"2.0"` {
		p.t.Fatalf("invalid AHP JSON-RPC frame: %s", data)
	}
	if string(frame["method"]) == `"action"` {
		var envelope types.ActionEnvelope
		if err := json.Unmarshal(frame["params"], &envelope); err != nil || envelope.Channel == "" || envelope.ServerSeq <= 0 || envelope.Action.Value == nil || len(frame["id"]) != 0 {
			p.t.Fatalf("invalid action notification: %s (%v)", data, err)
		}
		p.actions = append(p.actions, envelope)
	}
	return frame
}
