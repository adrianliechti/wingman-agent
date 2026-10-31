package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"

	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type actionIdentityAgent struct {
	*protocolAgent
	modelChanges atomic.Int32
}

func (a *actionIdentityAgent) SetModel(ctx context.Context, id, model string) error {
	a.modelChanges.Add(1)
	return a.protocolAgent.SetModel(ctx, id, model)
}

// Each deliberate configuration change has a new client sequence, even when
// its value was selected earlier. Only a retry of the same sequence is a retry.
func TestReviewAHPRepeatedConfigChangesTakeEffect(t *testing.T) {
	for _, channel := range []string{"chat", "session"} {
		t.Run(channel, func(t *testing.T) {
			s, b, _, c, ch := reviewSessionsAttach(t)
			observer := reviewSessionsObserver(t, s.ahp, "1.1.0", ch.chat.Resource, ch.session.resource)
			for i, model := range []string{"one", "two", "one"} {
				value, _ := json.Marshal(model)
				config := map[string]json.RawMessage{"model": value}
				params := types.DispatchActionParams{Channel: ch.chat.Resource, ClientSeq: int64(i + 1), Action: types.StateAction{Value: &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: config}}}
				if channel == "session" {
					params.Channel = ch.session.resource
					params.Action = types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: config}}
				}
				raw, err := json.Marshal(params)
				if err != nil {
					t.Fatal(err)
				}
				observer.dispatch(raw)
				for _, envelope := range reviewSessionsDrain(t, observer) {
					if envelope.RejectionReason != nil {
						t.Fatal(*envelope.RejectionReason)
					}
				}
				if got := b.settings(c.ref.SessionID).Model; got != model {
					t.Fatalf("fresh change %d: selected model %q, backend kept %q", i+1, model, got)
				}
			}
		})
	}
}

func TestReviewAHPRepeatedQueueEditsReachBackend(t *testing.T) {
	s, b, a := newProtocolServer(t)
	// A recovered queue stays paused, so its content can be inspected without
	// timing a running agent or letting the edited input start.
	a.queue.Inputs = []code.TurnInput{{ID: "queued", Content: []harness.Content{{Text: "original"}}}}
	c := b.session("saved")
	c.load()
	ch, err := s.ahpHost().attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	observer := reviewSessionsObserver(t, s.ahp, "1.1.0", ch.chat.Resource)
	for i, text := range []string{"first edit", "second edit", "first edit"} {
		params := types.DispatchActionParams{Channel: ch.chat.Resource, ClientSeq: int64(i + 1), Action: types.StateAction{Value: &types.ChatPendingMessageSetAction{
			Type: types.ActionTypeChatPendingMessageSet, Id: "queued", Kind: types.PendingMessageKindQueued,
			Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}},
		}}}
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		observer.dispatch(raw)
		for _, envelope := range reviewSessionsDrain(t, observer) {
			if envelope.RejectionReason != nil {
				t.Fatal(*envelope.RejectionReason)
			}
		}
		snapshot := b.turns.Snapshot(c.ref.SessionID)
		if len(snapshot.Inputs) != 1 || snapshot.Inputs[0].Input.Content[0].Text != text {
			t.Fatalf("fresh queue edit %d: wanted %q, backend queue = %+v", i+1, text, snapshot.Inputs)
		}
	}
}

func TestReviewAHPRepeatedUnattendedChangesTakeEffect(t *testing.T) {
	s, b, base := newProtocolServer(t)
	agent := &reviewSessionsModeAgent{protocolAgent: base}
	b.agent = agent
	c := b.session("saved")
	c.load()
	ch, err := s.ahpHost().attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	observer := reviewSessionsObserver(t, s.ahp, "1.1.0", ch.chat.Resource)
	for i, enabled := range []bool{false, true, false} {
		value, _ := json.Marshal(enabled)
		raw, err := json.Marshal(types.DispatchActionParams{Channel: ch.chat.Resource, ClientSeq: int64(i + 1), Action: types.StateAction{Value: &types.ChatConfigChangedAction{
			Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"unattended": value},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		observer.dispatch(raw)
		for _, envelope := range reviewSessionsDrain(t, observer) {
			if envelope.RejectionReason != nil {
				t.Fatal(*envelope.RejectionReason)
			}
		}
		if got := agent.Unattended(c.ref.SessionID); got != enabled {
			t.Fatalf("fresh change %d: unattended=%v, want %v", i+1, got, enabled)
		}
	}
}

func TestAHPActionRetriesDoNotReplayOlderEdits(t *testing.T) {
	for _, kind := range []string{"config", "queue"} {
		t.Run(kind, func(t *testing.T) {
			s, b, base := newProtocolServer(t)
			base.queue.Inputs = []code.TurnInput{{ID: "queued", Content: []harness.Content{{Text: "original"}}}}
			agent := &actionIdentityAgent{protocolAgent: base}
			b.agent = agent
			c := b.session("saved")
			c.load()
			ch, err := s.ahpHost().attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			observer := reviewSessionsObserver(t, s.ahp, "1.1.0", ch.chat.Resource)
			dispatch := func(peer *ahpConnection, seq int64, action types.StateAction) {
				t.Helper()
				raw, err := json.Marshal(types.DispatchActionParams{Channel: ch.chat.Resource, ClientSeq: seq, Action: action})
				if err != nil {
					t.Fatal(err)
				}
				peer.dispatch(raw)
				acknowledged := false
				for _, envelope := range reviewSessionsDrain(t, peer) {
					if envelope.RejectionReason != nil {
						t.Fatal(*envelope.RejectionReason)
					}
					acknowledged = acknowledged || envelope.Origin != nil && envelope.Origin.ClientId == peer.id && envelope.Origin.ClientSeq == seq
				}
				if !acknowledged {
					t.Fatal("action was not acknowledged")
				}
			}
			action := func(text string) types.StateAction {
				if kind == "config" {
					value, _ := json.Marshal(text)
					return types.StateAction{Value: &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"model": value}}}
				}
				return types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Id: "queued", Kind: types.PendingMessageKindQueued, Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
			}
			check := func(want string, changes int32) {
				t.Helper()
				got := b.settings(c.ref.SessionID).Model
				if kind == "queue" {
					got = b.turns.Snapshot(c.ref.SessionID).Inputs[0].Input.Content[0].Text
				} else if count := agent.modelChanges.Load(); count != changes {
					t.Fatalf("model changed %d times, want %d", count, changes)
				}
				s.ahp.mu.Lock()
				projected := string(ch.chat.Config.Values["model"])
				if kind == "queue" {
					projected = ch.chat.QueuedMessages[0].Message.Text
				} else {
					_ = json.Unmarshal(ch.chat.Config.Values["model"], &projected)
				}
				s.ahp.mu.Unlock()
				if got != want || projected != want {
					t.Fatalf("backend=%q projected=%q, want %q", got, projected, want)
				}
			}
			dispatch(observer, 1, action("one"))
			dispatch(observer, 2, action("two"))
			dispatch(observer, 1, action("one")) // Lost acknowledgement, retried after a newer edit.
			check("two", 2)
			// Other actions can evict the wire acknowledgement while the domain
			// receipt is still retained. The retry must preserve newer state here too.
			for i := range ahpAckLimit {
				dispatch(observer, int64(i+3), types.StateAction{Value: &types.ChatDraftChangedAction{Type: types.ActionTypeChatDraftChanged}})
			}
			dispatch(observer, 1, action("one"))
			check("two", 2)
			// Sequence numbers belong to the client: another client's first action
			// must not collide with either retained receipt.
			other := reviewSessionsObserver(t, s.ahp, "1.1.1", ch.chat.Resource)
			dispatch(other, 1, action("one"))
			check("one", 3)
		})
	}
}
