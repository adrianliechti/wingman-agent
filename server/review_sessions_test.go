package server

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func reviewSessionsAttach(t *testing.T) (*Server, *backendRuntime, *protocolAgent, *sessionController, *Chat) {
	t.Helper()
	s, b, a := newProtocolServer(t)
	c := b.session("saved")
	c.load()
	ch, err := s.Project.attachSession(c, s.Project.canonicalURI("one", "saved"), "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s, b, a, c, ch
}

// reviewSessionsObserver subscribes a connection without a socket to channels.
func reviewSessionsObserver(t *testing.T, h *Project, version string, channels ...string) *connection {
	conn := &connection{host: h.host, writer: newWSClient(nil), id: "observer-" + version, version: version, subscriptions: map[string]bool{}}
	t.Cleanup(func() { h.host.mu.Lock(); delete(h.host.clients, conn); h.host.mu.Unlock() })
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	for _, channel := range channels {
		conn.subscriptions[channel] = true
	}
	h.host.clients[conn] = struct{}{}
	return conn
}

func reviewSessionsDrain(t *testing.T, conn *connection) []types.ActionEnvelope {
	t.Helper()
	var actions []types.ActionEnvelope
	for {
		select {
		case data := <-conn.writer.outbox:
			var frame struct {
				Method string               `json:"method"`
				Params types.ActionEnvelope `json:"params"`
			}
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Method == "action" {
				actions = append(actions, frame.Params)
			}
		default:
			return actions
		}
	}
}

func reviewSessionsTypes(actions []types.ActionEnvelope) []string {
	names := []string{}
	for _, action := range actions {
		data, _ := json.Marshal(action.Action)
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &head)
		names = append(names, head.Type)
	}
	return names
}

// reviewSessionsReproject projects an unchanged controller again and returns
// what that published; a stable projection publishes nothing.
func reviewSessionsReproject(t *testing.T, c *sessionController, ch *Chat, observer *connection) []string {
	t.Helper()
	reviewSessionsDrain(t, observer)
	c.mu.Lock()
	ch.project.host.mu.Lock()
	ch.syncProjection(c, nil)
	ch.project.host.mu.Unlock()
	c.mu.Unlock()
	return reviewSessionsTypes(reviewSessionsDrain(t, observer))
}

func reviewSessionsInput(c *sessionController, id, text, origin string, state code.TurnInputState) {
	input := code.TurnInput{ID: id, Origin: origin, Content: []harness.Content{{Text: text}}}
	if origin == "task" {
		input.Content[0].Hidden = true
	}
	entry := turnQueueEntry(input, state, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &entry})
}

func reviewSessionsPrompt(t *testing.T, b *backendRuntime, c *sessionController, kind string) (string, chan Command) {
	t.Helper()
	before := map[string]bool{}
	c.mu.Lock()
	for id := range c.prompts {
		before[id] = true
	}
	c.mu.Unlock()
	reply := make(chan Command, 1)
	go func() {
		r, _ := b.prompt(code.WithSessionID(t.Context(), c.ref.SessionID), kind, "Proceed?", nil)
		reply <- r
	}()
	var id string
	waitProtocol(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for candidate := range c.prompts {
			if !before[candidate] {
				id = candidate
				return true
			}
		}
		return false
	})
	return id, reply
}

func reviewSessionsDispatch(t *testing.T, client *sdk.Client, channel string, seq int64, action any) {
	t.Helper()
	var value types.StateAction
	data, _ := json.Marshal(action)
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: channel, ClientSeq: seq, Action: value}); err != nil {
		t.Fatal(err)
	}
}

// reviewSessionsAgent runs each turn until the test releases its input ID.
type reviewSessionsAgent struct {
	*protocolAgent
	gates     sync.Map
	modelGate chan struct{} // when set, SetModel waits for it
	inModel   chan struct{}
}

func (a *reviewSessionsAgent) gate(id string) chan struct{} {
	gate, _ := a.gates.LoadOrStore(id, make(chan struct{}))
	return gate.(chan struct{})
}

func (a *reviewSessionsAgent) Send(ctx context.Context, _ string, input []harness.Content) (iter.Seq2[harness.Message, error], error) {
	id := harness.InputIDFromContext(ctx)
	a.sent.Add(1)
	a.mu.Lock()
	a.messages = append(a.messages, harness.Message{Role: harness.RoleUser, InputID: id, Content: harness.CloneContent(input)})
	a.mu.Unlock()
	reply := harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "working on " + id, TextID: "answer-" + id}}}
	return func(yield func(harness.Message, error) bool) {
		if !yield(reply, nil) {
			return
		}
		select {
		case <-a.gate(id):
		case <-ctx.Done():
			yield(harness.Message{}, ctx.Err())
			return
		}
		a.mu.Lock()
		a.messages = append(a.messages, reply)
		a.mu.Unlock()
	}, nil
}

func (a *reviewSessionsAgent) SetModel(ctx context.Context, id, value string) error {
	if a.modelGate != nil {
		a.inModel <- struct{}{}
		<-a.modelGate
	}
	return a.protocolAgent.SetModel(ctx, id, value)
}

func reviewSessionsReceipt(c *sessionController, id string) (reserved, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := c.receipts[id]
	if record == nil {
		return false, false
	}
	select {
	case <-record.done:
		return true, true
	default:
		return true, false
	}
}

// Bug: a stop that loses the race to its turn's end is rejected, yet its queued CancelAll still runs and cancels the next turn the user never stopped; cancel must be scoped to the turn it names.
func TestReviewSessionsLostCancelDoesNotStopTheNextTurn(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &reviewSessionsAgent{protocolAgent: &protocolAgent{}, modelGate: make(chan struct{}), inModel: make(chan struct{}, 1)}
	s.Project.backends["review"] = &backendEntry{runtime: s.Project.bindBackend("review", agent)}
	var releaseModel sync.Once
	t.Cleanup(func() { releaseModel.Do(func() { close(agent.modelGate) }) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "stopper")
	session := "ahp-session:/race"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("review")}, nil); err != nil {
		t.Fatal(err)
	}
	chat := ahp.ChatURI(session)
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn-1", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	reviewSessionsDispatch(t, client, chat, 2, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "turn-2", Message: types.Message{Text: "next", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}})
	until(t, state, sub, func(types.ActionEnvelope) bool { return len(state.QueuedMessages) == 1 })

	// Another client's model switch holds the controller while the stop arrives.
	other := protocolTestClient(t, endpoint, "configurer")
	reviewSessionsDispatch(t, other, chat, 1, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"model": json.RawMessage(`"slow"`)}})
	<-agent.inModel
	reviewSessionsDispatch(t, client, chat, 3, &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "turn-1"})
	s.Project.host.mu.Lock()
	c := s.Project.host.chats[chat].controller
	s.Project.host.mu.Unlock()
	stop := commandID("cancel", "turn-1")
	waitProtocol(t, func() bool { reserved, _ := reviewSessionsReceipt(c, stop); return reserved })

	close(agent.gate("turn-1"))
	ack := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 3 })
	if ack.RejectionReason == nil {
		t.Fatal("the stop was acknowledged although turn-1 had finished")
	}
	until(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn != nil && state.ActiveTurn.Id == "turn-2" })
	releaseModel.Do(func() { close(agent.modelGate) })
	waitProtocol(t, func() bool { _, done := reviewSessionsReceipt(c, stop); return done })
	close(agent.gate("turn-2"))
	end := until(t, state, sub, func(e types.ActionEnvelope) bool {
		switch a := e.Action.Value.(type) {
		case *types.ChatTurnCompleteAction:
			return a.TurnId == "turn-2"
		case *types.ChatTurnCancelledAction:
			return a.TurnId == "turn-2" && e.Origin == nil
		}
		return false
	})
	if _, cancelled := end.Action.Value.(*types.ChatTurnCancelledAction); cancelled {
		t.Fatal("a stop rejected for turn-1 cancelled turn-2")
	}
}

// Bug: a turnStarted action on an idle chat whose recovered queue is paused resumes that queue and runs the recovered message first; the requested turn is queued and its acknowledgement is withheld until it eventually starts.
func TestReviewSessionsTurnStartedIsNotQueuedBehindARecoveredQueue(t *testing.T) {
	s, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	agent.queue = code.TurnQueueState{Inputs: []code.TurnInput{{ID: "recovered", Content: []harness.Content{{Text: "from before the restart"}}}}}
	client := protocolTestClient(t, endpoint, "ide")
	chat := ahp.ChatURI(s.Project.canonicalURI("one", "saved"))
	state, sub := subscribeChat(t, client, chat)
	if state.ActiveTurn != nil || len(state.QueuedMessages) != 1 {
		t.Fatalf("recovered queue = %+v", state.QueuedMessages)
	}
	send(t, client, chat, "fresh", 1)
	first := until(t, state, sub, func(e types.ActionEnvelope) bool {
		_, started := e.Action.Value.(*types.ChatTurnStartedAction)
		return started || e.Origin != nil && e.Origin.ClientSeq == 1
	})
	if first.RejectionReason != nil {
		return
	}
	if started := first.Action.Value.(*types.ChatTurnStartedAction); started.TurnId != "fresh" {
		t.Fatalf("turnStarted(fresh) was accepted but %q started instead", started.TurnId)
	}
}

// Bug: while a background-task input (no visible user message) is active, the projection renames the previous completed turn into the active turn, so that turn vanishes from history.
func TestReviewSessionsTaskTurnKeepsThePreviousTurn(t *testing.T) {
	s, _, _, c, ch := reviewSessionsAttach(t)
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtTextDelta, ID: "a1", Text: "answer"})
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputCompleted)
	reviewSessionsInput(c, "task-1", "background agent finished", "task", code.TurnInputActive)
	s.Project.host.mu.Lock()
	defer s.Project.host.mu.Unlock()
	if len(ch.chat.Turns) != 1 || ch.chat.Turns[0].Id != "t1" {
		active := ""
		if ch.chat.ActiveTurn != nil {
			active = ch.chat.ActiveTurn.Id + " " + ch.chat.ActiveTurn.Message.Text
		}
		t.Fatalf("completed turn t1 left history while a task ran: turns=%d active=%q", len(ch.chat.Turns), active)
	}
}

// Bug: finishing a turn truncates and reloads the whole history, and the reloaded turn has lost the duration the completion carried.
func TestReviewSessionsTurnCompletionKeepsDurationWithoutReload(t *testing.T) {
	s, _, _, c, ch := reviewSessionsAttach(t)
	observer := reviewSessionsObserver(t, s.Project, "1.1.0", ch.chat.Resource)
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtTextDelta, ID: "a1", Text: "answer"})
	reviewSessionsDrain(t, observer)
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputCompleted)
	published := reviewSessionsTypes(reviewSessionsDrain(t, observer))
	s.Project.host.mu.Lock()
	defer s.Project.host.mu.Unlock()
	if slices.Contains(published, string(types.ActionTypeChatTruncated)) || len(ch.chat.Turns) != 1 || ch.chat.Turns[0].Duration == nil {
		t.Fatalf("completion published %v; turns=%+v", published, ch.chat.Turns)
	}
}

// Bug: prompt parts are appended after every entry of their turn, so each part that follows an answered approval truncates and replays the whole active turn.
func TestReviewSessionsPartsAfterAnApprovalDoNotReplayTheTurn(t *testing.T) {
	s, b, _, c, ch := reviewSessionsAttach(t)
	observer := reviewSessionsObserver(t, s.Project, "1.1.0", ch.chat.Resource)
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtToolCall, ID: "run", Name: "shell", Kind: "execute", Args: `{"command":"ls"}`})
	id, reply := reviewSessionsPrompt(t, b, c, PromptKindConfirm)
	if err := c.resolvePrompt(Command{PromptID: id, Action: "accept"}); err != nil {
		t.Fatal(err)
	}
	<-reply
	c.apply(Frame{Type: EvtToolResult, ID: "run", Name: "shell", Kind: "execute", Args: `{"command":"ls"}`, Content: "ok"})
	reviewSessionsDrain(t, observer)
	c.apply(Frame{Type: EvtTextDelta, ID: "after", Text: "done"})
	if published := reviewSessionsTypes(reviewSessionsDrain(t, observer)); slices.Contains(published, string(types.ActionTypeChatTruncated)) {
		t.Fatalf("one new text part replayed the active turn: %v", published)
	}
}

// reviewSessionsModeAgent mirrors the builtin agent's legacy shortcut: the
// "unattended" mode is stored as agent mode with unattended enabled.
type reviewSessionsModeAgent struct {
	*protocolAgent
	mode       string
	unattended bool
}

func (a *reviewSessionsModeAgent) Modes(string) ([]code.Mode, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return []code.Mode{{ID: code.AgentModeID, Name: "Agent"}, {ID: code.PlanModeID, Name: "Plan"}}, cmpOr(a.mode, code.AgentModeID)
}

func (a *reviewSessionsModeAgent) SetMode(_ context.Context, _, mode string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if mode == code.UnattendedModeID {
		a.unattended, mode = true, code.AgentModeID
	}
	a.mode = mode
	return nil
}

func (a *reviewSessionsModeAgent) Unattended(string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unattended
}

func (a *reviewSessionsModeAgent) SetUnattended(_ context.Context, _ string, enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unattended = enabled
	return nil
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// Bug: a config change the backend normalizes is acknowledged by echoing the client's raw values after the host's own replace, so every subscriber ends with a value the host does not hold and is never corrected.
func TestReviewSessionsNormalizedConfigReachesSubscribers(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &reviewSessionsModeAgent{protocolAgent: &protocolAgent{}}
	s.Project.backends["modes"] = &backendEntry{runtime: s.Project.bindBackend("modes", agent)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "settings")
	session := "ahp-session:/modes"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("modes")}, nil); err != nil {
		t.Fatal(err)
	}
	chat := ahp.ChatURI(session)
	observer := protocolTestClient(t, endpoint, "observer")
	state, sub := subscribeChat(t, observer, chat)
	reviewSessionsDispatch(t, client, chat, 1, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"mode": json.RawMessage(`"unattended"`)}})
	until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientId == "settings" })
	s.Project.host.mu.Lock()
	host := string(s.Project.host.chats[chat].chat.Config.Values["mode"])
	s.Project.host.mu.Unlock()
	if seen := string(state.Config.Values["mode"]); seen != host {
		t.Fatalf("subscriber holds mode %s, host holds %s", seen, host)
	}
}

// Bug: disposing a session keeps its alias on disk and in memory, so the record leaks forever and the URI stays taken after a restart.
func TestReviewSessionsDisposeSessionForgetsItsAlias(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "disposer")
	session := "ahp-session:/short-lived"
	create(t, client, session)
	if err := client.Request(t.Context(), "disposeSession", types.DisposeSessionParams{Channel: session}, nil); err != nil {
		t.Fatal(err)
	}
	dir, err := s.Project.aliasDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aliasPath(dir, sessionAlias{Resource: session})); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposed session's alias file remains: %v", err)
	}
	s.Project.host.mu.Lock()
	_, aliased := s.Project.host.aliases[session]
	s.Project.host.mu.Unlock()
	if aliased {
		t.Fatal("disposed session kept its alias")
	}
}

// Bug: createSession accepts a URI in the host's canonical namespace, binding a new native session to another session's canonical URI and making that session unreachable.
func TestReviewSessionsCreateSessionCannotClaimACanonicalURI(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "squatter")
	existing := s.Project.canonicalURI("one", "saved")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = client.Request(ctx, "createSession", types.CreateSessionParams{Channel: existing, Provider: new("one")}, nil)
	_, ch, err := s.Project.host.ensureSession(ahp.ChatURI(existing))
	if err != nil {
		t.Fatal(err)
	}
	if id := ch.controller.ref.SessionID; id != "saved" {
		t.Fatalf("canonical URI of native session saved now opens %q", id)
	}
}

// Bug: the canonical chat URI of a forked native session redirects to the parent session, whose default chat clients then open instead of the fork.
func TestReviewSessionsCanonicalForkURIRedirectsToTheForkChat(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &turnForkingAgent{protocolAgent: &protocolAgent{}}
	s.Project.backends["fork"] = &backendEntry{runtime: s.Project.bindBackend("fork", agent)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "forks")
	session := "ahp-session:/parent"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("fork")}, nil); err != nil {
		t.Fatal(err)
	}
	main := ahp.ChatURI(session)
	state, sub := subscribeChat(t, client, main)
	send(t, client, main, "turn-1", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: "ahp-chat:/branch", Source: &types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: main, TurnId: "turn-1"}}}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.Subscribe(ctx, ahp.ChatURI(s.Project.canonicalURI("fork", "fork-1")))
	var rpc *sdk.RPCError
	var data struct {
		Resource string `json:"resource"`
	}
	if !errors.As(err, &rpc) || json.Unmarshal(rpc.Data, &data) != nil {
		t.Fatalf("expected a redirect, got %v", err)
	}
	if data.Resource != "ahp-chat:/branch" {
		t.Fatalf("canonical fork chat redirects to %q", data.Resource)
	}
}

// Bug: syncChats reads the default chat controller's settings under the host lock only, racing with refreshSettings, which writes them under the controller lock.
func TestReviewSessionsSessionActionsDoNotRaceSettingsRefresh(t *testing.T) {
	s, _, agent, c, ch := reviewSessionsAttach(t)
	conn := &connection{host: s.Project.host, id: "racer"}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			agent.mu.Lock()
			agent.model = "model-" + string(rune('a'+i%26))
			agent.mu.Unlock()
			c.refreshSettings()
		}
	})
	wg.Go(func() {
		for i := range 200 {
			action := types.StateAction{Value: &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: i%2 == 0}}
			if err := conn.executeAction(ch.session, nil, types.DispatchActionParams{Channel: ch.session.resource, ClientSeq: int64(i + 1), Action: action}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
}

// Bug: an input request raised while the chat has no active turn is attached to no turn and never listed in the session's inputNeeded, so no AHP client can see or answer it and the asking tool blocks.
func TestReviewSessionsQuestionOutsideATurnIsSurfaced(t *testing.T) {
	s, b, _, c, ch := reviewSessionsAttach(t)
	id, _ := reviewSessionsPrompt(t, b, c, PromptKindAsk)
	s.Project.host.mu.Lock()
	defer s.Project.host.mu.Unlock()
	for _, request := range ch.session.state.InputNeeded {
		if r, ok := request.Value.(*types.SessionChatInputRequest); ok && r.Id == id {
			return
		}
	}
	t.Fatalf("question %s is invisible: inputNeeded=%+v active=%v", id, ch.session.state.InputNeeded, ch.chat.ActiveTurn != nil)
}

// Bug: naming a new session after its first message renames only the session; its default chat keeps "New session" until a restart, where the catalog title appears instead. The default chat now inherits the session's title.
func TestReviewSessionsDefaultChatInheritsTheSessionTitle(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "titles")
	session := "ahp-session:/titled"
	chat := create(t, client, session)
	state, sub := subscribeChat(t, client, chat)
	reviewSessionsDispatch(t, client, chat, 1, &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "first", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "Fix the login bug", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}})
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	s.Project.host.mu.Lock()
	defer s.Project.host.mu.Unlock()
	a := s.Project.host.sessions[session]
	if summary := a.summary(); summary.Title != "Fix the login bug" || summary.Chats[0].Title != "" || a.chats[0].chat.Title != "" {
		t.Fatalf("session %q lists its default chat as %q, which calls itself %q", summary.Title, summary.Chats[0].Title, a.chats[0].chat.Title)
	}
}

type reviewSessionsCompactForkAgent struct {
	*compactingProtocolAgent
}

func (a *reviewSessionsCompactForkAgent) ForkSession(_ context.Context, id, _ string) (string, error) {
	return "fork-of-" + id, nil
}

// Mirrors session.ForkThrough: the journal knows only the agent's user inputs.
func (a *reviewSessionsCompactForkAgent) ForkSessionThrough(_ context.Context, id, inputID, _ string) (string, error) {
	for _, m := range a.Messages(id) {
		if m.Role == harness.RoleUser && m.InputID == inputID {
			return "fork-through-" + inputID, nil
		}
	}
	return "", errors.New("session " + id + " has no input " + inputID)
}

// Bug: compaction turns are offered as chat turns but are not inputs of the agent's journal, so forking through one fails.
func TestReviewSessionsForkThroughACompactionTurn(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &reviewSessionsCompactForkAgent{&compactingProtocolAgent{protocolAgent: &protocolAgent{}, focus: make(chan string, 4), release: make(chan error, 4)}}
	agent.release <- nil
	s.Project.backends["compact"] = &backendEntry{runtime: s.Project.bindBackend("compact", agent)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "compactor")
	session := "ahp-session:/compacted"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("compact")}, nil); err != nil {
		t.Fatal(err)
	}
	main := ahp.ChatURI(session)
	state, sub := subscribeChat(t, client, main)
	for i, text := range []string{"first", "/compact", "second"} {
		turn := "turn-" + text
		if text == "/compact" {
			turn = "compaction"
		}
		reviewSessionsDispatch(t, client, main, int64(i+1), &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: turn, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}})
		until(t, state, sub, func(e types.ActionEnvelope) bool {
			done, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
			return ok && done.TurnId == turn
		})
	}
	if len(state.Turns) != 3 || state.Turns[1].Id != "compaction" {
		t.Fatalf("turns = %+v", state.Turns)
	}
	err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: "ahp-chat:/after-compaction", Source: &types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: main, TurnId: "compaction"}}}, nil)
	if err != nil {
		t.Fatalf("forking through the compaction turn failed: %v", err)
	}
}

func reviewSessionsDiverged(c *sessionController, ch *Chat) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch.project.host.mu.Lock()
	defer ch.project.host.mu.Unlock()
	turns, active := ch.transcript(c)
	if !reflect.DeepEqual(turns, ch.chat.Turns) {
		a, _ := json.Marshal(turns)
		b, _ := json.Marshal(ch.chat.Turns)
		return "turns\n" + string(a) + "\n" + string(b)
	}
	if (active == nil) != (ch.chat.ActiveTurn == nil) || active != nil && !reflect.DeepEqual(active.ResponseParts, ch.chat.ActiveTurn.ResponseParts) {
		a, _ := json.Marshal(active)
		b, _ := json.Marshal(ch.chat.ActiveTurn)
		return "active\n" + string(a) + "\n" + string(b)
	}
	return ""
}

// The host's reduced state must equal its projection after every kind of
// change, or each later sync truncates and replays.
func TestReviewSessionsProjectionIsAFixedPoint(t *testing.T) {
	s, b, _, c, ch := reviewSessionsAttach(t)
	observer := reviewSessionsObserver(t, s.Project, "1.1.0", ch.chat.Resource)
	step := func(name string) {
		t.Helper()
		if d := reviewSessionsDiverged(c, ch); d != "" {
			t.Fatalf("%s diverged: %s", name, d)
		}
		if p := reviewSessionsReproject(t, c, ch, observer); len(p) > 0 {
			t.Fatalf("%s reprojected: %v", name, p)
		}
	}
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtReasoningDelta, ID: "r1", Text: "thinking"})
	step("reasoning")
	c.apply(Frame{Type: EvtTextDelta, ID: "a1", Text: "answer"})
	step("text after reasoning")
	c.apply(Frame{Type: EvtImage, Content: "data:image/png;base64,iVBORw0KGgo="})
	step("image")
	c.apply(Frame{Type: EvtToolCall, ID: "shot", Name: "screenshot", Kind: "read", Args: `{}`})
	c.apply(Frame{Type: EvtToolResult, ID: "shot", Name: "screenshot", Kind: "read", Args: `{}`, Content: "captured"})
	step("tool")
	steer := turnQueueEntry(code.TurnInput{ID: "s1", Content: []harness.Content{{Text: "also check tests"}}}, code.TurnInputSteered, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &steer})
	step("steering")
	ask, reply := reviewSessionsPrompt(t, b, c, PromptKindAsk)
	step("ask pending")
	_ = c.resolvePrompt(Command{PromptID: ask, Action: "accept", Content: map[string]any{}})
	<-reply
	step("ask answered")
	c.apply(Frame{Type: EvtStreamReset})
	step("stream reset")
	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputCompleted)
	step("completed")
	reviewSessionsInput(c, "t2", "again", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtTextDelta, ID: "a2", Text: "partial"})
	c.apply(Frame{Type: EvtError, Message: "boom"})
	reviewSessionsInput(c, "t2", "again", "user", code.TurnInputFailed)
	step("failed")
	reviewSessionsInput(c, "t3", "third", "user", code.TurnInputActive)
	step("after failure")
}

// Bug: text attachments are appended to the command text that also becomes the input's display text, so once the turn completes the user's message shows the attachment's full contents.
func TestReviewSessionsAttachmentContentStaysOutOfTheMessageText(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "attacher")
	chat := create(t, client, "ahp-session:/attachments")
	state, sub := subscribeChat(t, client, chat)
	attachment := types.MessageAttachment{Value: &types.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: "text/plain", Label: "notes.txt", Data: "UFJJVkFURSBGSUxFIEJPRFk="}}
	reviewSessionsDispatch(t, client, chat, 1, &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "explain", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "explain", Origin: types.MessageOrigin{Kind: types.MessageKindUser}, Attachments: []types.MessageAttachment{attachment}}})
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	agent.mu.Lock()
	sent := ""
	for _, message := range agent.messages {
		for _, content := range message.Content {
			sent += content.Text // the attachment arrives as context of its own
		}
	}
	agent.mu.Unlock()
	if !strings.Contains(sent, "PRIVATE FILE BODY") {
		t.Fatalf("the agent did not receive the attachment: %q", sent)
	}
	snapshot, _ := subscribeChat(t, protocolTestClient(t, endpoint, "reader"), chat)
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Message.Text != "explain" {
		t.Fatalf("message text became %q", snapshot.Turns[0].Message.Text)
	}
}

// Bug: controller command receipts are never evicted, so every action a client ever sent stays in memory for the host's lifetime although the docs promise bounded receipts.
func TestReviewSessionsCommandReceiptsAreBounded(t *testing.T) {
	_, b, _, c, _ := reviewSessionsAttach(t)
	if !b.Project.beginOperation() {
		t.Fatal("workspace closed")
	}
	defer b.Project.operations.Done()
	const commands = 10000
	for i := range commands {
		c.command(t.Context(), Command{ID: "answer-" + strconv.Itoa(i), Type: "prompt_response", PromptID: "gone", Action: "accept"})
	}
	c.mu.Lock()
	kept := len(c.receipts)
	c.mu.Unlock()
	if kept >= commands {
		t.Fatalf("%d receipts retained for %d commands", kept, commands)
	}
}

func TestReviewSessionsConcurrentCreateSessionCreatesOnce(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, agent := newProtocolServer(t)
	h := s.Project
	raw, _ := json.Marshal(types.CreateSessionParams{Channel: "ahp-session:/contended", Provider: new("one")})
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for range 16 {
		wg.Go(func() {
			_, err := h.createSession(raw, "client")
			var rpc *ahp.Error
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.As(err, &rpc) && rpc.Code == -32003:
			default:
				t.Error(err)
			}
		})
	}
	for i := range 8 {
		wg.Go(func() {
			other, _ := json.Marshal(types.CreateSessionParams{Channel: "ahp-session:/distinct-" + strconv.Itoa(i), Provider: new("one")})
			if _, err := h.createSession(other, "client"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if succeeded != 1 || agent.created.Load() != 9 {
		t.Fatalf("succeeded=%d created=%d", succeeded, agent.created.Load())
	}
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	natives := map[string]bool{}
	for _, alias := range h.host.aliases {
		if natives[alias.Session] {
			t.Fatalf("two URIs share native session %s", alias.Session)
		}
		natives[alias.Session] = true
	}
}

func TestReviewSessionsConcurrentReloadAttachesEachForkOnce(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &turnForkingAgent{protocolAgent: &protocolAgent{}}
	s.Project.backends["fork"] = &backendEntry{runtime: s.Project.bindBackend("fork", agent)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "forks")
	session := "ahp-session:/family"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("fork")}, nil); err != nil {
		t.Fatal(err)
	}
	main := ahp.ChatURI(session)
	state, sub := subscribeChat(t, client, main)
	send(t, client, main, "turn-1", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	chats := []string{main}
	for i := range 4 {
		chat := "ahp-chat:/branch-" + strconv.Itoa(i)
		if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: chat, Source: &types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: main, TurnId: "turn-1"}}}, nil); err != nil {
			t.Fatal(err)
		}
		chats = append(chats, chat)
	}

	other, _, _ := newProtocolServer(t)
	other.Project.backends["fork"] = &backendEntry{runtime: other.Project.bindBackend("fork", agent)}
	h := other.Project
	var wg sync.WaitGroup
	for i := range 24 {
		channel := chats[i%len(chats)]
		if i%6 == 0 {
			channel = session
		}
		wg.Go(func() {
			if _, _, err := h.host.ensureSession(channel); err != nil {
				t.Error(channel, err)
			}
		})
	}
	wg.Wait()
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	a := h.host.sessions[session]
	got := []string{}
	for _, ch := range a.chats {
		got = append(got, ch.chat.Resource)
	}
	listed := []string{}
	for _, summary := range a.state.Chats {
		listed = append(listed, summary.Resource)
	}
	if !slices.Equal(got, chats) || !slices.Equal(listed, chats) || len(h.host.chats) != len(chats) {
		t.Fatalf("chats = %v, state = %v, want %v", got, listed, chats)
	}
}

// A retried send must not run twice or leave a queued message behind, whether
// it arrives while its turn runs or after it finished.
func TestReviewSessionsRetriedPendingSendNeverDuplicates(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "web")
	chat := create(t, client, "ahp-session:/retries")
	state, sub := subscribeChat(t, client, chat)
	send := &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "message", Message: types.Message{Text: "do it", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}
	reviewSessionsDispatch(t, client, chat, 1, send)
	until(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn != nil && state.ActiveTurn.Id == "message" })
	for seq := int64(2); seq <= 4; seq++ {
		if seq == 4 {
			close(agent.gate)
			until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
		}
		reviewSessionsDispatch(t, client, chat, seq, send)
		ack := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == seq })
		if ack.RejectionReason != nil {
			t.Fatalf("retry %d rejected: %s", seq, *ack.RejectionReason)
		}
		until(t, state, sub, func(e types.ActionEnvelope) bool {
			removed, ok := e.Action.Value.(*types.ChatPendingMessageRemovedAction)
			return ok && removed.Id == "message"
		})
	}
	if agent.sent.Load() != 1 || len(state.QueuedMessages) != 0 {
		t.Fatalf("sent=%d queued=%+v", agent.sent.Load(), state.QueuedMessages)
	}
	snapshot, _ := subscribeChat(t, protocolTestClient(t, endpoint, "reader"), chat)
	if len(snapshot.QueuedMessages) != 0 || len(snapshot.Turns) != 1 {
		t.Fatalf("host kept a ghost: %+v", snapshot)
	}
}

func TestReviewSessionsHostileActionsAreRejected(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	defer close(agent.gate)
	client := protocolTestClient(t, endpoint, "hostile")
	session := "ahp-session:/hostile"
	chat := create(t, client, session)
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "running", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	user := types.MessageOrigin{Kind: types.MessageKindUser}
	started := time.Now().UTC().Format(time.RFC3339Nano)
	cases := []struct {
		channel string
		action  any
	}{
		{session, &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "on-session", StartedAt: started, Message: types.Message{Text: "x", Origin: user}}},
		{chat, &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "busy", StartedAt: started, Message: types.Message{Text: "x", Origin: user}}},
		{chat, &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "someone-else"}},
		{chat, &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: "running", ToolCallId: "approval:nope", Approved: true}},
		{chat, &types.ChatInputCompletedAction{Type: types.ActionTypeChatInputCompleted, RequestId: "nope", Response: types.ChatInputResponseKindAccept}},
		{chat, &types.ChatInputAnswerChangedAction{Type: types.ActionTypeChatInputAnswerChanged, RequestId: "nope", QuestionId: "q"}},
		{chat, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "agent", Message: types.Message{Text: "x", Origin: types.MessageOrigin{Kind: types.MessageKindAgent}}}},
		{chat, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Message: types.Message{Text: "x", Origin: user}}},
		{session, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "on-session", Message: types.Message{Text: "x", Origin: user}}},
		{chat, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "bad-base64", Message: types.Message{Text: "x", Origin: user, Attachments: []types.MessageAttachment{{Value: &types.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: "text/plain", Label: "x", Data: "%%%"}}}}}},
		{chat, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "zip", Message: types.Message{Text: "x", Origin: user, Attachments: []types.MessageAttachment{{Value: &types.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: "application/zip", Label: "x", Data: "AA=="}}}}}},
		{chat, &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "outside", Message: types.Message{Text: "x", Origin: user, Attachments: []types.MessageAttachment{{Value: &types.MessageResourceAttachment{Type: "resource", Uri: "file:///etc/passwd", Label: "passwd"}}}}}},
		{chat, &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: types.PendingMessageKindQueued, Id: "never-queued"}},
		{chat, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"temperature": json.RawMessage(`"hot"`)}}},
		{chat, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"model": json.RawMessage(`42`)}}},
		{chat, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"unattended": json.RawMessage(`"yes"`)}}},
		{chat, &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: map[string]json.RawMessage{}}},
		{session, &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{}}},
		{session, &types.SessionTitleChangedAction{Type: types.ActionTypeSessionTitleChanged, Title: "   "}},
		{session, &types.SessionActiveClientSetAction{Type: types.ActionTypeSessionActiveClientSet, ActiveClient: types.SessionActiveClient{ClientId: "impostor"}}},
		{session, &types.ChatDraftChangedAction{Type: types.ActionTypeChatDraftChanged}},
		{chat, &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: true}},
		{"ahp-session:/wingman/!!/!!", &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: true}},
	}
	snapshot := func() string {
		chat, _ := subscribeChat(t, protocolTestClient(t, endpoint, "reader"), chat)
		chat.ModifiedAt = ""
		data, _ := json.Marshal(chat)
		return string(data)
	}
	before := snapshot()
	events := client.Events()
	defer events.Close()
	for i, tc := range cases {
		seq := int64(i + 10)
		reviewSessionsDispatch(t, client, tc.channel, seq, tc.action)
		timer := time.NewTimer(5 * time.Second)
		for acked := false; !acked; {
			select {
			case event := <-events.Events():
				action, ok := event.Event.(sdk.SubscriptionEventAction)
				if !ok || action.Envelope.Origin == nil || action.Envelope.Origin.ClientSeq != seq {
					continue
				}
				if action.Envelope.RejectionReason == nil {
					data, _ := json.Marshal(tc.action)
					t.Fatalf("accepted on %s: %s", tc.channel, data)
				}
				acked = true
			case <-timer.C:
				t.Fatalf("case %d was never acknowledged", i)
			}
		}
		timer.Stop()
	}
	// An action for a chat that does not exist is ignored, without an echo.
	reviewSessionsDispatch(t, client, "ahp-chat:/nowhere", 900, &types.ChatDraftChangedAction{Type: types.ActionTypeChatDraftChanged})
	for quiet := time.After(300 * time.Millisecond); quiet != nil; {
		select {
		case event := <-events.Events():
			if action, ok := event.Event.(sdk.SubscriptionEventAction); ok && action.Envelope.Origin != nil && action.Envelope.Origin.ClientSeq == 900 {
				t.Fatal("an action for an unknown chat was echoed")
			}
		case <-quiet:
			quiet = nil
		}
	}
	if after := snapshot(); before != after || agent.sent.Load() != 1 {
		t.Fatalf("rejected actions changed the chat:\n%s\n%s", before, after)
	}
}

func TestReviewSessionsForkCutsAtTheChosenTurnWhileTheSourceRuns(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &turnForkingAgent{protocolAgent: &protocolAgent{}}
	s.Project.backends["fork"] = &backendEntry{runtime: s.Project.bindBackend("fork", agent)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "brancher")
	session := "ahp-session:/running-source"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("fork")}, nil); err != nil {
		t.Fatal(err)
	}
	main := ahp.ChatURI(session)
	state, sub := subscribeChat(t, client, main)
	send(t, client, main, "turn-1", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	agent.gate = make(chan struct{})
	send(t, client, main, "turn-2", 2)
	until(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn != nil && state.ActiveTurn.Id == "turn-2" })
	fork := func(chat, turn string) error {
		return client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: chat, Source: &types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: main, TurnId: turn}}}, nil)
	}
	if err := fork("ahp-chat:/from-running", "turn-2"); err == nil {
		t.Fatal("forked through a running turn")
	}
	if err := fork("ahp-chat:/while-running", "turn-1"); err != nil {
		t.Fatal(err)
	}
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	if err := fork("ahp-chat:/after", "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := fork("ahp-chat:/wingman/squat", "turn-2"); err == nil {
		t.Fatal("a fork claimed a canonical chat URI")
	}
	if err := fork(main, "turn-2"); err == nil {
		t.Fatal("a fork reused the default chat URI")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !slices.Equal(agent.forks, []string{"session-1>turn-1", "session-1>"}) {
		t.Fatalf("forks = %v", agent.forks)
	}
}

func TestReviewSessionsCorruptAliasFilesAreSkipped(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, _ := newProtocolServer(t)
	h := s.Project
	if err := h.saveAlias(sessionAlias{Resource: "ahp-session:/kept", Backend: "one", Session: "saved"}); err != nil {
		t.Fatal(err)
	}
	dir, err := h.aliasDirectory()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"garbage.json":    "{not json",
		"empty.json":      "",
		"scheme.json":     `{"resource":"https://evil.example/x","backend":"one","session":"saved"}`,
		"no-session.json": `{"resource":"ahp-session:/half","backend":"one"}`,
		"query.json":      `{"resource":"ahp-session:/q?x=1","backend":"one","session":"saved"}`,
		"notes.txt":       `{"resource":"ahp-session:/txt","backend":"one","session":"saved"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	other, _, _ := newProtocolServer(t)
	restarted := other.Project
	restarted.host.mu.Lock()
	aliases := len(restarted.host.aliases) + len(restarted.host.chatAliases)
	restarted.host.mu.Unlock()
	if aliases != 1 {
		t.Fatalf("loaded %d aliases", aliases)
	}
	if a, ch, err := restarted.host.ensureSession(ahp.ChatURI("ahp-session:/kept")); err != nil || a.resource != "ahp-session:/kept" || ch.controller.ref.SessionID != "saved" {
		t.Fatalf("valid alias lost: %v", err)
	}
}
