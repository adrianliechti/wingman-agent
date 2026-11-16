package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// drain applies what arrives until the chat is quiet for 400ms.
func drain(state *types.ChatState, sub *sdk.Subscription, seen []types.ActionEnvelope) []types.ActionEnvelope {
	for {
		select {
		case event := <-sub.Events():
			if action, ok := event.(sdk.SubscriptionEventAction); ok {
				seen = append(seen, action.Envelope)
				if action.Envelope.RejectionReason == nil {
					sdk.ApplyActionToChat(state, action.Envelope.Action)
				}
			}
		case <-time.After(400 * time.Millisecond):
			return seen
		}
	}
}

func kinds(seen []types.ActionEnvelope) (kinds []string) {
	for _, e := range seen {
		kinds = append(kinds, fmt.Sprintf("%T", e.Action.Value))
	}
	return kinds
}

// A failed turn keeps its error once the next turn starts.
func TestAHPFailedTurnKeepsItsErrorAfterTheNextTurn(t *testing.T) {
	model, c := truncateTest(t)
	model.mu.Lock()
	model.fail = "broken question"
	model.mu.Unlock()
	c.complete("one", "broken question")
	before := truncateJSON(t, c.state.Turns[0])
	if !strings.Contains(before, "the model refused") {
		t.Fatalf("no failure: %s", before)
	}
	var seen []types.ActionEnvelope
	*c.seq++
	seq := *c.seq
	started := types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: "two", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: "fine question", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
	if err := c.client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: c.state.Resource, ClientSeq: seq, Action: started}); err != nil {
		t.Fatal(err)
	}
	until(t, c.state, c.events, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return truncateTurns(c.state, "one", "two")
	})
	after := truncateJSON(t, c.state.Turns[0])
	fresh := truncateJSON(t, c.snapshot().Turns[0])
	if after != before || fresh != before {
		t.Fatalf("failed turn changed:\nbefore %s\nmirror %s\nsnapshot %s", before, after, fresh)
	}
}

// A queue that cannot be saved after a successful turn does not make that turn fail.
func TestAHPQueueSaveFailureLeavesTheFinishedTurn(t *testing.T) {
	_, _, agent, endpoint := protocolTestServer(t)
	agent.mu.Lock()
	agent.failSave = true
	agent.mu.Unlock()
	client := protocolTestClient(t, endpoint, "persist")
	chat := create(t, client, "ahp-session:/persist")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn-1", 1)
	var seen []types.ActionEnvelope
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		_, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
		return ok
	})
	seen = drain(state, sub, seen)
	if len(state.Turns) != 1 || state.Turns[0].State != types.TurnStateComplete {
		t.Fatalf("the completed turn became %s: %s", state.Turns[0].State, truncateJSON(t, state.Turns[0]))
	}
}

// Answering an approval leaves the active turn as the host projects it.
func TestAHPApprovalDoesNotRestartTheTurn(t *testing.T) {
	_, b, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "approval")
	chat := create(t, client, "ahp-session:/approval")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	replies := make(chan bool, 1)
	go func() {
		ok, _ := b.Confirm(code.WithSessionID(t.Context(), "session-1"), "Run it?")
		replies <- ok
	}()
	var callID string
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		if ready, ok := e.Action.Value.(*types.ChatToolCallReadyAction); ok {
			callID = ready.ToolCallId
			return true
		}
		return false
	})
	confirm := types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: "turn", ToolCallId: callID, Approved: true, SelectedOptionId: new("once")}}}
	if err := client.Notify(t.Context(), "dispatchAction", confirm); err != nil {
		t.Fatal(err)
	}
	var seen []types.ActionEnvelope
	ack := until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return e.Origin != nil && e.Origin.ClientSeq == 2
	})
	if ack.RejectionReason != nil {
		t.Fatal(*ack.RejectionReason)
	}
	<-replies
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		_, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
		return ok
	})
	seen = drain(state, sub, seen)
	for _, e := range seen {
		if _, cut := e.Action.Value.(*types.ChatTruncatedAction); cut {
			t.Fatalf("the host reloaded the chat after an approval: %v", kinds(seen))
		}
	}
}

// An answer that the host files under a companion field leaves the
// reduced request as the host projects it.
func TestAHPCompanionAnswerMatchesTheProjection(t *testing.T) {
	_, b, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "companion")
	chat := create(t, client, "ahp-session:/companion")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	replies := make(chan tool.ElicitResult, 1)
	go func() {
		result, _ := b.Elicit(code.WithSessionID(t.Context(), "session-1"), tool.ElicitRequest{Message: "Choose", Fields: []tool.ElicitField{
			{Name: "choice", Type: "string", Enum: []string{"one", "two"}, Strict: true},
			{Name: "other", Type: "string", CustomAnswerFor: "choice"},
		}})
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
	answer := types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatInputCompletedAction{Type: types.ActionTypeChatInputCompleted, RequestId: requestID, Response: types.ChatInputResponseKindAccept, Answers: map[string]types.ChatInputAnswer{"choice": {Value: &types.ChatInputAnswered{State: types.ChatInputAnswerStateSubmitted, Value: types.ChatInputAnswerValue{Value: &types.ChatInputSelectedAnswerValue{Kind: types.ChatInputAnswerValueKindSelected, Value: "three"}}}}}}}}
	if err := client.Notify(t.Context(), "dispatchAction", answer); err != nil {
		t.Fatal(err)
	}
	var seen []types.ActionEnvelope
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return e.Origin != nil && e.Origin.ClientSeq == 2
	})
	reply := <-replies
	t.Logf("agent got %+v", reply)
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		_, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
		return ok
	})
	seen = drain(state, sub, seen)
	completions := 0
	for _, e := range seen {
		if _, ok := e.Action.Value.(*types.ChatInputCompletedAction); ok {
			completions++
		}
		if _, cut := e.Action.Value.(*types.ChatTruncatedAction); cut {
			t.Fatalf("the host reloaded the chat after the answer: %v", kinds(seen))
		}
	}
	if completions != 1 {
		t.Fatalf("%d input completions: %v", completions, kinds(seen))
	}
}

// toolStuckAgent's turn calls a tool that never returns, then waits for the
// gate or ends with fail.
type toolStuckAgent struct {
	*protocolAgent
	fail error
}

func (a *toolStuckAgent) Send(ctx context.Context, _ string, input []harness.Content) (iter.Seq2[harness.Message, error], error) {
	a.mu.Lock()
	a.messages = append(a.messages, harness.Message{Role: harness.RoleUser, InputID: harness.InputIDFromContext(ctx), Content: harness.CloneContent(input)})
	a.mu.Unlock()
	return func(yield func(harness.Message, error) bool) {
		call := harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{ToolCall: &harness.ToolCall{ID: "call-" + harness.InputIDFromContext(ctx), Name: "shell", Kind: "execute", Args: `{"command":"sleep 100"}`}}}}
		a.mu.Lock()
		a.messages = append(a.messages, call)
		a.mu.Unlock()
		if !yield(call, nil) {
			return
		}
		if a.fail != nil {
			yield(harness.Message{}, a.fail)
			return
		}
		<-ctx.Done()
		yield(harness.Message{}, ctx.Err())
	}, nil
}

func stuckTool(t *testing.T, fail error) (*sdk.Client, string, *types.ChatState, *sdk.Subscription, *backendRuntime) {
	t.Helper()
	s, _, _, endpoint := protocolTestServer(t)
	agent := &toolStuckAgent{protocolAgent: &protocolAgent{}, fail: fail}
	b := s.Project.bindBackend("stuck", agent)
	s.Project.backends["stuck"] = &backendEntry{runtime: b}
	client := protocolTestClient(t, endpoint, "stuck")
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: "ahp-session:/stuck", Provider: new("stuck")}, nil); err != nil {
		t.Fatal(err)
	}
	chat := ahp.ChatURI("ahp-session:/stuck")
	state, sub := subscribeChat(t, client, chat)
	return client, chat, state, sub, b
}

// A turn cancelled while a tool runs ends as clients reduced it.
func TestAHPCancelledTurnWithARunningToolNeedsNoReload(t *testing.T) {
	client, chat, state, sub, _ := stuckTool(t, nil)
	send(t, client, chat, "turn", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatTurnCancelledAction{Type: types.ActionTypeChatTurnCancelled, TurnId: "turn"}}}); err != nil {
		t.Fatal(err)
	}
	seen := drain(state, sub, nil)
	t.Logf("turn: %s", truncateJSON(t, state.Turns))
	for _, e := range seen {
		if _, cut := e.Action.Value.(*types.ChatTruncatedAction); cut {
			t.Fatalf("the host reloaded the chat after the cancellation: %v", kinds(seen))
		}
	}
}

// A turn that fails while a tool runs is not replayed when the next one starts.
func TestAHPFailedTurnWithARunningToolIsNotReplayed(t *testing.T) {
	client, chat, state, sub, _ := stuckTool(t, errors.New("provider broke"))
	send(t, client, chat, "turn", 1)
	drain(state, sub, nil)
	failed := truncateJSON(t, state.Turns)
	// The next turn projects the failed one again.
	send(t, client, chat, "next", 2)
	seen := drain(state, sub, nil)
	for _, e := range seen {
		if _, cut := e.Action.Value.(*types.ChatTruncatedAction); cut {
			t.Fatalf("the failed turn was replayed: %v", kinds(seen))
		}
	}
	if got := truncateJSON(t, state.Turns[:1]); got != failed {
		t.Fatalf("the failed turn changed:\n%s\n%s", failed, got)
	}
}

// modeAgent has modes per session; switching the first session to plan waits for block.
type modeAgent struct {
	*protocolAgent
	modes   map[string]string
	block   chan struct{}
	blocked chan struct{}
}

func (a *modeAgent) Modes(id string) ([]code.Mode, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	mode := a.modes[id]
	if mode == "" {
		mode = "agent"
	}
	return []code.Mode{{ID: "agent", Name: "Agent"}, {ID: "plan", Name: "Plan"}}, mode
}

func (a *modeAgent) SetEffort(context.Context, string, string) error { return nil }

func (a *modeAgent) SetMode(_ context.Context, id, mode string) error {
	if id == "session-1" && mode == "plan" && a.block != nil {
		a.blocked <- struct{}{}
		<-a.block
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.modes[id] = mode
	return nil
}

// A chat keeps the value a client set on it while a session change runs.
func TestAHPSessionChangeKeepsAChatsOwnValue(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &modeAgent{protocolAgent: &protocolAgent{}, modes: map[string]string{}}
	b := s.Project.bindBackend("modes", agent)
	s.Project.backends["modes"] = &backendEntry{runtime: b}
	client := protocolTestClient(t, endpoint, "modes")
	session := "ahp-session:/modes"
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: session, Provider: new("modes")}, nil); err != nil {
		t.Fatal(err)
	}
	sibling := "ahp-chat:/modes-sibling"
	if err := client.Request(t.Context(), "createChat", types.CreateChatParams{Channel: session, Chat: sibling}, nil); err != nil {
		t.Fatal(err)
	}
	_, sessionSub, err := client.Subscribe(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	defer sessionSub.Close()
	siblingState, siblingSub := subscribeChat(t, client, sibling)
	agent.block, agent.blocked = make(chan struct{}), make(chan struct{})
	plan := map[string]json.RawMessage{"mode": json.RawMessage(`"plan"`)}
	// The session switches to plan; its default chat's switch is slow.
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: session, ClientSeq: 1, Action: types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: plan}}}); err != nil {
		t.Fatal(err)
	}
	<-agent.blocked
	// Meanwhile a client keeps the sibling in agent mode.
	other := protocolTestClient(t, endpoint, "other")
	if err := other.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: sibling, ClientSeq: 1, Action: types.StateAction{Value: &types.ChatConfigChangedAction{Type: types.ActionTypeChatConfigChanged, Config: map[string]json.RawMessage{"mode": json.RawMessage(`"agent"`)}}}}); err != nil {
		t.Fatal(err)
	}
	ack := until(t, siblingState, siblingSub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientId == "other" })
	t.Logf("the sibling's change was answered with %s", truncateJSON(t, ack))
	close(agent.block)
	untilSession(t, sessionSub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientId == "modes" })
	drain(siblingState, siblingSub, nil)
	_, native := agent.Modes("session-2")
	if native != "agent" || string(siblingState.Config.Values["mode"]) != `"agent"` {
		t.Fatalf("the sibling set agent but runs %q and shows %s", native, truncateJSON(t, siblingState.Config.Values))
	}
}

// slowHistoryAgent's history read waits for release while held is set, as a
// slow turn finalization does.
type slowHistoryAgent struct {
	*protocolAgent
	held    chan struct{}
	release chan struct{}
}

func (a *slowHistoryAgent) Messages(id string) []harness.Message {
	a.mu.Lock()
	held := a.held
	a.held = nil
	a.mu.Unlock()
	if held != nil {
		close(held)
		<-a.release
	}
	return a.protocolAgent.Messages(id)
}

// A turn a client starts while the last one settles starts once it has,
// and its action is answered.
func TestAHPTurnStartedWhileTheLastTurnSettlesStarts(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &slowHistoryAgent{protocolAgent: &protocolAgent{gate: make(chan struct{})}, release: make(chan struct{})}
	b := s.Project.bindBackend("slow", agent)
	s.Project.backends["slow"] = &backendEntry{runtime: b}
	client := protocolTestClient(t, endpoint, "slow")
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: "ahp-session:/slow", Provider: new("slow")}, nil); err != nil {
		t.Fatal(err)
	}
	chat := ahp.ChatURI("ahp-session:/slow")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn-1", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool { return state.ActiveTurn != nil })
	held := make(chan struct{})
	agent.mu.Lock()
	agent.held = held
	agent.mu.Unlock()
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	<-held // the finished turn is being finalized
	send(t, client, chat, "turn-2", 2)
	time.Sleep(50 * time.Millisecond)
	close(agent.release)
	var seen []types.ActionEnvelope
	echo := until(t, state, sub, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return e.Origin != nil && e.Origin.ClientSeq == 2
	})
	if echo.RejectionReason != nil {
		t.Fatalf("the started turn was rejected: %s", *echo.RejectionReason)
	}
	for _, e := range seen {
		if set, ok := e.Action.Value.(*types.ChatPendingMessageSetAction); ok && set.Id == "turn-2" {
			t.Fatalf("the started turn was queued: %v", kinds(seen))
		}
	}
	if state.ActiveTurn == nil || state.ActiveTurn.Id != "turn-2" {
		t.Fatalf("turn-2 did not start: %s", truncateJSON(t, state.ActiveTurn))
	}
}

func TestAHPNormalizedSessionValueKeepsTheDefaultChatFollowing(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &reviewSessionsModeAgent{protocolAgent: &protocolAgent{}}
	s.Project.backends["modes"] = &backendEntry{runtime: s.Project.bindBackend("modes", agent)}
	client := protocolTestClient(t, endpoint, "settings")
	session := "ahp-session:/normalized"
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{Channel: session, Provider: new("modes")}, nil); err != nil {
		t.Fatal(err)
	}
	_, sub, err := client.Subscribe(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for seq, mode := range []string{code.UnattendedModeID, code.PlanModeID} {
		value, _ := json.Marshal(mode)
		if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: session, ClientSeq: int64(seq + 1), Action: types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: map[string]json.RawMessage{"mode": value}}}}); err != nil {
			t.Fatal(err)
		}
		echo := untilSession(t, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == int64(seq+1) })
		t.Logf("%s answered with %s", mode, truncateJSON(t, echo.Action))
	}
	agent.mu.Lock()
	mode := agent.mode
	agent.mu.Unlock()
	h := s.Project
	h.host.mu.Lock()
	sessionMode := string(h.host.sessions[session].state.Config.Values["mode"])
	overlay := truncateJSON(t, h.host.chats[ahp.ChatURI(session)].chat.Config.Values)
	h.host.mu.Unlock()
	if mode != code.PlanModeID {
		t.Fatalf("the session says mode %s but its default chat runs %q (overlay %s)", sessionMode, mode, overlay)
	}
}

// Steering a running turn takes stays part of that turn once it ends.
func TestAHPSteeringStaysInItsTurnAfterTheTurnEnds(t *testing.T) {
	_, _, owner, _, _ := clientToolTurn(t)
	steer := types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindSteering, Id: "steer-1", Message: types.Message{Text: "also check line 4", Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
	if echo := owner.dispatch(steer); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	running := owner.call().Value.(*types.ToolCallRunningState)
	result := types.ToolCallResult{Success: true, PastTenseMessage: types.NewStringOrMarkdownPlain("Read"), Content: clientToolText("line 3 is selected")}
	if echo := owner.dispatch(types.StateAction{Value: &types.ChatToolCallCompleteAction{Type: types.ActionTypeChatToolCallComplete, TurnId: "turn", ToolCallId: running.ToolCallId, Result: result}}); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	var seen []types.ActionEnvelope
	owner.until(func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		_, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
		return ok
	})
	seen = drain(owner.chat, owner.events, seen)
	var ids []string
	for _, turn := range owner.chat.Turns {
		ids = append(ids, turn.Id)
	}
	t.Logf("turns: %v", ids)
	if len(owner.chat.Turns) != 1 {
		t.Fatalf("the steered turn became %d turns: %s", len(owner.chat.Turns), truncateJSON(t, owner.chat.Turns))
	}
	for _, e := range seen {
		if _, cut := e.Action.Value.(*types.ChatTruncatedAction); cut {
			t.Fatalf("the host reloaded the chat after a steered turn: %v", kinds(seen))
		}
	}
}

// An approval that background work raises during a turn stays answerable
// after that turn ends.
func TestAHPApprovalOutlivingItsTurnStaysAnswerable(t *testing.T) {
	s, b, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	client := protocolTestClient(t, endpoint, "late")
	chat := create(t, client, "ahp-session:/late")
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn", 1)
	until(t, state, sub, func(types.ActionEnvelope) bool {
		return state.ActiveTurn != nil && len(state.ActiveTurn.ResponseParts) > 0
	})
	// A background task of the session asks, while the turn runs.
	replies := make(chan bool, 1)
	go func() {
		ok, _ := b.Confirm(code.WithSessionID(context.Background(), "session-1"), "Let the task write?")
		replies <- ok
	}()
	var callID string
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		if ready, ok := e.Action.Value.(*types.ChatToolCallReadyAction); ok {
			callID = ready.ToolCallId
			return true
		}
		return false
	})
	// The turn ends without the answer; the task still waits.
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		_, ok := e.Action.Value.(*types.ChatTurnCompleteAction)
		return ok
	})
	drain(state, sub, nil)
	h := s.Project
	h.host.mu.Lock()
	needed := truncateJSON(t, h.host.sessions["ahp-session:/late"].state.InputNeeded)
	h.host.mu.Unlock()
	t.Logf("session inputNeeded: %s", needed)
	t.Logf("last turn parts: %s", truncateJSON(t, state.Turns[0].ResponseParts))
	confirm := types.DispatchActionParams{Channel: chat, ClientSeq: 2, Action: types.StateAction{Value: &types.ChatToolCallConfirmedAction{Type: types.ActionTypeChatToolCallConfirmed, TurnId: "turn", ToolCallId: callID, Approved: true}}}
	if err := client.Notify(t.Context(), "dispatchAction", confirm); err != nil {
		t.Fatal(err)
	}
	echo := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 2 })
	request := strings.TrimPrefix(callID, "approval:")
	answer := types.DispatchActionParams{Channel: chat, ClientSeq: 3, Action: types.StateAction{Value: &types.ChatInputCompletedAction{Type: types.ActionTypeChatInputCompleted, RequestId: request, Response: types.ChatInputResponseKindAccept}}}
	if err := client.Notify(t.Context(), "dispatchAction", answer); err != nil {
		t.Fatal(err)
	}
	echo2 := until(t, state, sub, func(e types.ActionEnvelope) bool { return e.Origin != nil && e.Origin.ClientSeq == 3 })
	select {
	case <-replies:
	case <-time.After(time.Second):
		t.Fatalf("the waiting approval cannot be answered: inputNeeded %s; confirm rejected %v; input rejected %v", needed, *echo.RejectionReason, *echo2.RejectionReason)
	}
}
