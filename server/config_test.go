package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A chat's config holds only what it sets over its session's values.
func TestAHPChatConfigIsAnOverlayOnItsSession(t *testing.T) {
	_, c := truncateTest(t)
	session, sibling := "ahp-session:/truncate", "ahp-chat:/sibling"
	if err := c.client.Request(t.Context(), "createChat", types.CreateChatParams{Channel: session, Chat: sibling}, nil); err != nil {
		t.Fatal(err)
	}
	_, sessionEvents, err := c.client.Subscribe(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sessionEvents.Close)
	dispatch := func(channel string, events *ahp.Subscription, action types.StateAction) {
		t.Helper()
		*c.seq++
		seq := *c.seq
		if err := c.client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: channel, ClientSeq: seq, Action: action}); err != nil {
			t.Fatal(err)
		}
		for timeout := time.After(5 * time.Second); ; {
			select {
			case event := <-events.Events():
				if action, ok := event.(ahp.SubscriptionEventAction); ok && action.Envelope.Origin != nil && action.Envelope.Origin.ClientSeq == seq {
					if action.Envelope.RejectionReason != nil {
						t.Fatal(*action.Envelope.RejectionReason)
					}
					if channel == c.state.Resource {
						ahp.ApplyActionToChat(c.state, action.Envelope.Action)
					}
					return
				}
				if action, ok := event.(ahp.SubscriptionEventAction); ok && channel == c.state.Resource && action.Envelope.RejectionReason == nil {
					ahp.ApplyActionToChat(c.state, action.Envelope.Action)
				}
			case <-timeout:
				t.Fatalf("no answer to %s", channel)
			}
		}
	}
	mode := func(value string) map[string]json.RawMessage {
		data, _ := json.Marshal(value)
		return map[string]json.RawMessage{"mode": data}
	}
	chatConfig := func(channel string, config map[string]json.RawMessage, replace bool) {
		t.Helper()
		action := &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: config}
		if replace {
			action.Replace = new(true)
		}
		events := c.events
		if channel != c.state.Resource {
			_, events = subscribeChat(t, c.client, channel)
		}
		dispatch(channel, events, types.StateAction{Value: action})
	}
	sessionMode := func(value string) {
		t.Helper()
		dispatch(session, sessionEvents, types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: mode(value)}})
	}
	// Fresh snapshots show the session's mode and what each chat sets over it.
	expect := func(step, sessionValue string, own, siblings map[string]json.RawMessage) {
		t.Helper()
		snapshot := func(uri string) types.SnapshotState {
			t.Helper()
			result, sub, err := c.other.Subscribe(t.Context(), uri)
			if err != nil {
				t.Fatal(err)
			}
			sub.Close()
			return result.Snapshot.State
		}
		if got := string(snapshot(session).Session.Config.Values["mode"]); got != `"`+sessionValue+`"` {
			t.Fatalf("%s: session mode %s, want %q", step, got, sessionValue)
		}
		for uri, want := range map[string]map[string]json.RawMessage{c.state.Resource: own, sibling: siblings} {
			if got := snapshot(uri).Chat.Config.Values; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %s sets %s, want %s", step, uri, truncateJSON(t, got), truncateJSON(t, want))
			}
		}
	}
	none := map[string]json.RawMessage{}
	expect("at first", "agent", none, none)
	chatConfig(c.state.Resource, mode("plan"), false)
	expect("a chat's own value", "agent", mode("plan"), none)
	sessionMode("plan")
	expect("a session change", "plan", mode("plan"), none)
	sessionMode("agent")
	expect("a value the chat keeps", "agent", mode("plan"), none)
	chatConfig(sibling, mode("agent"), false)
	sessionMode("plan")
	expect("a value the sibling set, equal to the session's", "plan", mode("plan"), mode("agent"))
	chatConfig(c.state.Resource, none, true)
	expect("a reset", "plan", none, mode("agent"))
	if mirror, fresh := truncateJSON(t, c.state.Config), truncateJSON(t, c.snapshot().Config); mirror != fresh {
		t.Fatalf("the mirror holds %s, a fresh snapshot %s", mirror, fresh)
	}
}
