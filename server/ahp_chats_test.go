package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type turnForkingAgent struct {
	*protocolAgent
	forks []string // source>through for each fork
}

func (a *turnForkingAgent) ForkSession(_ context.Context, id, _ string) (string, error) {
	return a.fork(id, "")
}

func (a *turnForkingAgent) ForkSessionThrough(_ context.Context, id, inputID, _ string) (string, error) {
	return a.fork(id, inputID)
}

func (a *turnForkingAgent) fork(id, through string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forks = append(a.forks, id+">"+through)
	return fmt.Sprintf("fork-%d", len(a.forks)), nil
}

func (a *turnForkingAgent) ListSessions(context.Context) ([]code.SessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sessions := []code.SessionInfo{}
	for i := range a.created.Load() {
		sessions = append(sessions, code.SessionInfo{ID: fmt.Sprintf("session-%d", i+1), Title: "Main"})
	}
	for i := range a.forks {
		sessions = append(sessions, code.SessionInfo{ID: fmt.Sprintf("fork-%d", i+1), Title: "Fork of Main"})
	}
	return sessions, nil
}

func ahpUntilSession(t *testing.T, sub *ahp.Subscription, predicate func(types.ActionEnvelope) bool) types.ActionEnvelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed")
			}
			if action, ok := event.(ahp.SubscriptionEventAction); ok && predicate(action.Envelope) {
				return action.Envelope
			}
		case <-timer.C:
			t.Fatal("timed out waiting for session action")
		}
	}
}

func TestAHPForkChatsLiveInTheirSession(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	agent := &turnForkingAgent{protocolAgent: &protocolAgent{}}
	s.backends["fork"] = &backendEntry{runtime: s.bindBackend("fork", agent)}
	client := ahpTestClient(t, endpoint, "forks")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session := "ahp-session:/forks"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: ahpPtr("fork")}, nil); err != nil {
		t.Fatal(err)
	}
	_, sessionSub, err := client.Subscribe(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer sessionSub.Close()
	main := ahpChatURI(session)
	state, sub := ahpSubscribeChat(t, client, main)
	for i, turn := range []string{"turn-1", "turn-2"} {
		ahpSend(t, client, main, turn, int64(i+1))
		ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	}

	fork := func(chat, turn string) error {
		return client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: chat, Source: &types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: main, TurnId: turn}}}, nil)
	}
	var rpc *ahp.RPCError
	if err := fork("ahp-chat:/bad", "turn-9"); !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeInvalidParams {
		t.Fatalf("unknown turn: %v", err)
	}
	if err := fork("ahp-chat:/early", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := fork("ahp-chat:/late", "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := fork("ahp-chat:/late", "turn-2"); !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeAlreadyExists {
		t.Fatalf("reused chat URI: %v", err)
	}
	// Only a fork from an earlier turn needs the native journal cut.
	if fmt.Sprint(agent.forks) != "[session-1>turn-1 session-1>]" {
		t.Fatalf("forks = %v", agent.forks)
	}
	added := ahpUntilSession(t, sessionSub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.SessionChatAddedAction); return ok })
	summary := added.Action.Value.(*types.SessionChatAddedAction).Summary
	if origin, ok := summary.Origin.Value.(*types.ChatForkOrigin); summary.Resource != "ahp-chat:/early" || !ok || origin.TurnId != "turn-1" || origin.Chat != main {
		t.Fatalf("added chat = %+v", summary)
	}
	if forked, _ := ahpSubscribeChat(t, client, "ahp-chat:/early"); forked.Origin == nil || forked.Title != "Fork of Main" {
		t.Fatalf("fork chat = %+v", forked)
	}

	var listed types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || len(listed.Items[0].Chats) != 3 || listed.Items[0].Chats[1].Resource != "ahp-chat:/early" {
		t.Fatalf("listed = %+v", listed.Items)
	}

	// A restarted host finds forks through their chat URI, in creation order.
	other, _, _ := newProtocolServer(t)
	other.backends["fork"] = &backendEntry{runtime: other.bindBackend("fork", agent)}
	a, ch, err := other.ahpHost().ensureSession("ahp-chat:/late")
	if err != nil {
		t.Fatal(err)
	}
	if a.resource != session || ch.controller.ref.SessionID != "fork-2" || len(a.chats) != 3 || a.chats[1].chat.Resource != "ahp-chat:/early" {
		t.Fatalf("reloaded session = %+v", a.state.Chats)
	}

	if err := client.Request(ctx, "disposeChat", types.DisposeChatParams{Channel: main}, nil); !errors.As(err, &rpc) {
		t.Fatalf("disposed the default chat: %v", err)
	}
	if err := client.Request(ctx, "disposeChat", types.DisposeChatParams{Channel: "ahp-chat:/early"}, nil); err != nil {
		t.Fatal(err)
	}
	ahpUntilSession(t, sessionSub, func(e types.ActionEnvelope) bool {
		removed, ok := e.Action.Value.(*types.SessionChatRemovedAction)
		return ok && removed.Chat == "ahp-chat:/early"
	})
	h := s.ahpHost()
	h.mu.Lock()
	_, aliased := h.chatAliases["ahp-chat:/early"]
	h.mu.Unlock()
	if aliased {
		t.Fatal("disposed fork kept its alias")
	}
	if err := client.Request(ctx, "disposeSession", types.DisposeSessionParams{Channel: session}, nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.chats) != 0 || !h.sessions[session].deleted {
		t.Fatalf("session chats survived disposal: %v", h.chats)
	}
}

func TestAHPCanonicalURIRedirectsToAlias(t *testing.T) {
	_, _, _, endpoint := ahpTestServer(t)
	client := ahpTestClient(t, endpoint, "redirect")
	ahpCreate(t, client, "ahp-session:/named")
	_, _, err := client.Subscribe(t.Context(), canonicalAHPURI("one", "session-1"))
	var rpc *ahp.RPCError
	var data struct {
		Resource string `json:"resource"`
	}
	if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeInvalidParams || json.Unmarshal(rpc.Data, &data) != nil || data.Resource != "ahp-session:/named" {
		t.Fatalf("expected a redirect to the alias, got %v", err)
	}
}

func TestAHPStopClearsQueueAndApprovalOptionsChooseScope(t *testing.T) {
	_, b, agent, endpoint := ahpTestServer(t)
	agent.gate = make(chan struct{})
	defer close(agent.gate)
	client := ahpTestClient(t, endpoint, "stop")
	chat := ahpCreate(t, client, "ahp-session:/stop")
	state, sub := ahpSubscribeChat(t, client, chat)
	ahpSend(t, client, chat, "turn-1", 1)
	ahpUntil(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn != nil })
	queue := types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "later", Message: types.Message{Text: "later", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}}
	if err := client.Notify(t.Context(), "dispatchAction", queue); err != nil {
		t.Fatal(err)
	}
	ahpUntil(t, state, sub, func(types.ActionEnvelope) bool { return len(state.QueuedMessages) == 1 })
	stop := types.DispatchActionParams{Channel: chat, ClientSeq: 3, Action: types.StateAction{Value: &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "turn-1"}}}
	if err := client.Notify(t.Context(), "dispatchAction", stop); err != nil {
		t.Fatal(err)
	}
	ahpUntil(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn == nil && len(state.QueuedMessages) == 0 })
	if len(b.turns.Snapshot("session-1").Inputs) != 0 {
		t.Fatal("stop left queued input behind")
	}
}
