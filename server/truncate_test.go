package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// truncateModel keeps what it was asked and can hold a reply back.
type truncateModel struct {
	mu       sync.Mutex
	requests []string
	hold     chan struct{} // while set, the replies to "slow" questions wait for it
	fail     string        // questions it refuses
}

func (m *truncateModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6-sol"}]}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, string(body))
	hold, fail := m.hold, m.fail
	m.mu.Unlock()
	if fail != "" && strings.Contains(string(body), fail) {
		http.Error(w, `{"error":{"message":"the model refused","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"answer","output_index":0,"content_index":0,"delta":"an answer"}`+"\n\n")
	w.(http.Flusher).Flush()
	if hold != nil && strings.Contains(string(body), "slow question") {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	fmt.Fprint(w, `data: {"type":"response.completed","sequence_number":2,"response":{"output":[{"type":"message","id":"answer","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"an answer","annotations":[]}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
}

// asked is the last request that carried a question.
func (m *truncateModel) asked(question string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range slices.Backward(m.requests) {
		if strings.Contains(v, question) {
			return v
		}
	}
	return ""
}

type truncateChat struct {
	t         *testing.T
	app       *Server
	client    *sdk.Client
	other     *sdk.Client
	directory string
	seq       *int64 // the client's, shared by its chats
	state     *types.ChatState
	events    *sdk.Subscription
}

// open creates another session for the same clients and follows its chat.
func (c *truncateChat) open(session string) *truncateChat {
	c.t.Helper()
	if err := c.client.Request(c.t.Context(), "createSession", types.CreateSessionParams{Channel: session, Provider: new("wingman"), WorkingDirectories: []string{c.directory}}, nil); err != nil {
		c.t.Fatal(err)
	}
	next := *c
	next.state, next.events = subscribeChat(c.t, c.client, ahp.ChatURI(session))
	return &next
}

func truncateTest(t *testing.T) (*truncateModel, *truncateChat) {
	t.Helper()
	testenv.WingmanHome(t)
	model := &truncateModel{}
	gateway := httptest.NewServer(model)
	t.Cleanup(gateway.Close)
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-6-sol")
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	c := &truncateChat{t: t, app: app, client: protocolTestClient(t, endpoint, ""), other: protocolTestClient(t, endpoint, ""), directory: app.Project.directoryURI(), seq: new(int64)}
	for id, client := range map[string]*sdk.Client{"editor": c.client, "reader": c.other} {
		if _, err := client.Initialize(ctx, id, []string{"1.1.0"}, []string{types.RootResourceURI}); err != nil {
			t.Fatal(err)
		}
	}
	return model, c.open("ahp-session:/truncate")
}

func (c *truncateChat) dispatch(action types.StateAction) (echo types.ActionEnvelope, seen []types.ActionEnvelope) {
	c.t.Helper()
	*c.seq++
	seq := *c.seq
	if err := c.client.Notify(c.t.Context(), "dispatchAction", types.DispatchActionParams{Channel: c.state.Resource, ClientSeq: seq, Action: action}); err != nil {
		c.t.Fatal(err)
	}
	echo = until(c.t, c.state, c.events, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return e.Origin != nil && e.Origin.ClientId == "editor" && e.Origin.ClientSeq == seq
	})
	return echo, seen
}

func (c *truncateChat) start(turn, text string, attachments ...types.MessageAttachment) types.ActionEnvelope {
	c.t.Helper()
	echo, _ := c.dispatch(types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: turn, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}, Attachments: attachments}}})
	return echo
}

func (c *truncateChat) complete(turn, text string, attachments ...types.MessageAttachment) {
	c.t.Helper()
	if echo := c.start(turn, text, attachments...); echo.RejectionReason != nil {
		c.t.Fatal(*echo.RejectionReason)
	}
	if !truncateTurns(c.state, turn) {
		until(c.t, c.state, c.events, func(types.ActionEnvelope) bool { return truncateTurns(c.state, turn) })
	}
}

// truncateTurns reports whether a chat's completed turns end with the given ones.
func truncateTurns(state *types.ChatState, ids ...string) bool {
	if len(state.Turns) < len(ids) || state.ActiveTurn != nil {
		return false
	}
	for i, turn := range state.Turns[len(state.Turns)-len(ids):] {
		if turn.Id != ids[i] {
			return false
		}
	}
	return true
}

// snapshot is the chat as a client that just subscribes sees it.
func (c *truncateChat) snapshot() *types.ChatState {
	c.t.Helper()
	state, sub := subscribeChat(c.t, c.other, c.state.Resource)
	sub.Close()
	return state
}

func truncateJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAHPTruncateKeepsHistoryThroughATurn(t *testing.T) {
	model, c := truncateTest(t)
	c.complete("one", "first question")
	c.complete("two", "second question")
	if echo, _ := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: new("unknown")}}); echo.RejectionReason == nil {
		t.Fatal("a cut after an unknown turn was accepted")
	}
	kept := truncateJSON(t, c.state.Turns[:1])
	echo, seen := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: new("one")}})
	if echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	// The cut is the client's own action; the host reloads nothing.
	if len(seen) != 1 || truncateJSON(t, c.state.Turns) != kept || truncateJSON(t, c.snapshot().Turns) != kept {
		t.Fatalf("%d actions left the mirror %s and the snapshot %s", len(seen), truncateJSON(t, c.state.Turns), truncateJSON(t, c.snapshot().Turns))
	}
	// The agent continues from the turn that was kept.
	c.complete("three", "third question")
	request := model.asked("third question")
	if !strings.Contains(request, "first question") || strings.Contains(request, "second question") {
		t.Fatalf("the model was asked: %s", request)
	}
	if !truncateTurns(c.state, "one", "three") || len(c.state.Turns) != 2 {
		t.Fatalf("turns after continuing: %s", truncateJSON(t, c.state.Turns))
	}
	// Each turn has the model's final counts, also the first after the cut.
	for _, turn := range c.state.Turns {
		if turn.Usage == nil || turn.Usage.OutputTokens == nil || *turn.Usage.OutputTokens != 2 {
			t.Fatalf("usage of %s: %s", turn.Id, truncateJSON(t, turn.Usage))
		}
	}
}

func TestAHPTruncateDropsTheActiveTurn(t *testing.T) {
	model, c := truncateTest(t)
	c.complete("one", "first question")
	model.mu.Lock()
	model.hold = make(chan struct{})
	model.mu.Unlock()
	c.start("two", "slow question")
	for deadline := time.Now().Add(5 * time.Second); model.asked("slow question") == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the model was never asked")
		}
	}
	if c.state.ActiveTurn == nil || c.state.ActiveTurn.Id != "two" {
		t.Fatalf("no active turn: %s", truncateJSON(t, c.state))
	}
	echo, _ := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated}})
	if echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	close(model.hold)
	// Nothing of the dropped turn arrives after the cut.
	quiet := time.NewTimer(300 * time.Millisecond)
	defer quiet.Stop()
	for waiting := true; waiting; {
		select {
		case event := <-c.events.Events():
			if action, ok := event.(sdk.SubscriptionEventAction); ok && strings.Contains(truncateJSON(t, action.Envelope.Action), `"turnId":"two"`) {
				t.Fatalf("the dropped turn continued: %s", truncateJSON(t, action.Envelope))
			}
		case <-quiet.C:
			waiting = false
		}
	}
	for name, state := range map[string]*types.ChatState{"mirror": c.state, "snapshot": c.snapshot()} {
		if state.ActiveTurn != nil || len(state.Turns) != 0 || !state.Status.Has(types.SessionStatusIdle) {
			t.Fatalf("%s after clearing: %s", name, truncateJSON(t, state))
		}
	}
	c.complete("three", "third question")
	if request := model.asked("third question"); strings.Contains(request, "first question") || strings.Contains(request, "slow question") {
		t.Fatalf("the model was asked: %s", request)
	}
}

func TestAHPTruncateKeepsAFailedTurnAsItWas(t *testing.T) {
	model, c := truncateTest(t)
	c.complete("one", "first question")
	model.mu.Lock()
	model.fail = "broken question"
	model.mu.Unlock()
	c.complete("two", "broken question")
	// The turn ends with its failure, not one it learns of later.
	if failed := truncateJSON(t, c.state.Turns[1]); c.state.Turns[1].State != types.TurnStateError || !strings.Contains(failed, "the model refused") {
		t.Fatalf("the turn did not fail as the model did: %s", failed)
	}
	kept := truncateJSON(t, c.state.Turns)
	echo, seen := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: new("two")}})
	if echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	if len(seen) != 1 || truncateJSON(t, c.state.Turns) != kept || truncateJSON(t, c.snapshot().Turns) != kept {
		t.Fatalf("%d actions; mirror %s; snapshot %s", len(seen), truncateJSON(t, c.state.Turns), truncateJSON(t, c.snapshot().Turns))
	}
}

func truncateSteer(id, text string) types.StateAction {
	return types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindSteering, Id: id, Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}}
}

// Steering sent to an idle chat waits for a turn to steer instead of starting one.
func TestAHPIdleSteeringWaitsForATurn(t *testing.T) {
	model, c := truncateTest(t)
	for _, action := range []types.StateAction{
		truncateSteer("first", "mind the tests"),
		truncateSteer("second", "keep it short"),
		truncateSteer("second", "keep it very short"),
		truncateSteer("third", "drop this"),
		{Value: &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: types.PendingMessageKindSteering, Id: "third"}},
	} {
		if echo, _ := c.dispatch(action); echo.RejectionReason != nil {
			t.Fatal(*echo.RejectionReason)
		}
	}
	waiting := func(state *types.ChatState) (ids []string) {
		for _, m := range state.QueuedMessages {
			ids = append(ids, m.Id+"="+m.Message.Text)
		}
		return ids
	}
	want := []string{"first=mind the tests", "second=keep it very short"}
	for name, state := range map[string]*types.ChatState{"mirror": c.state, "snapshot": c.snapshot()} {
		if !slices.Equal(waiting(state), want) || state.ActiveTurn != nil || len(state.Turns) != 0 {
			t.Fatalf("%s: %s", name, truncateJSON(t, state))
		}
	}
	if model.asked("mind the tests") != "" {
		t.Fatal("waiting steering reached the model")
	}
	// The next turn takes them.
	model.mu.Lock()
	model.hold = make(chan struct{})
	model.mu.Unlock()
	c.start("turn", "slow question")
	until(t, c.state, c.events, func(types.ActionEnvelope) bool {
		return len(c.state.QueuedMessages) == 0 && c.state.SteeringMessage == nil
	})
	close(model.hold)
	until(t, c.state, c.events, func(types.ActionEnvelope) bool { return truncateTurns(c.state, "turn") })
	if request := model.asked("keep it very short"); !strings.Contains(request, "mind the tests") || strings.Contains(request, "drop this") {
		t.Fatalf("the model was asked: %s", request)
	}
	if len(c.state.Turns) != 1 || !strings.Contains(truncateJSON(t, c.state.Turns[0].ResponseParts), "keep it very short") {
		t.Fatalf("the turn does not show the steering: %s", truncateJSON(t, c.state.Turns))
	}
}

func TestAHPWaitingSteeringBelongsToTheQueue(t *testing.T) {
	model, c := truncateTest(t)
	c.complete("one", "first question")
	for _, action := range []types.StateAction{
		truncateSteer("first", "mind the tests"),
		truncateSteer("second", "keep it short"),
		{Value: &types.ChatQueuedMessagesReorderedAction{Type: types.ActionTypeChatQueuedMessagesReordered, Order: []string{"second", "first"}}},
	} {
		if echo, _ := c.dispatch(action); echo.RejectionReason != nil {
			t.Fatal(*echo.RejectionReason)
		}
	}
	queued := func(state *types.ChatState) (ids []string) {
		for _, m := range state.QueuedMessages {
			ids = append(ids, m.Id)
		}
		return ids
	}
	for name, state := range map[string]*types.ChatState{"mirror": c.state, "snapshot": c.snapshot()} {
		if !slices.Equal(queued(state), []string{"second", "first"}) {
			t.Fatalf("%s: %s", name, truncateJSON(t, state.QueuedMessages))
		}
	}
	// The saved queue keeps the steering for a project that closes meanwhile.
	saved := func() []string {
		c.app.Project.host.mu.Lock()
		controller := c.app.Project.host.chats[c.state.Resource].controller
		c.app.Project.host.mu.Unlock()
		queue, err := controller.backend.agent.(code.TurnQueueStore).LoadTurnQueue(controller.ref.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, input := range queue.Inputs {
			ids = append(ids, input.ID)
		}
		return ids
	}
	if ids := saved(); !slices.Equal(ids, []string{"second", "first"}) {
		t.Fatalf("saved queue: %v", ids)
	}
	// Clearing the conversation drops what waited with the history.
	if echo, _ := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated}}); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	for name, state := range map[string]*types.ChatState{"mirror": c.state, "snapshot": c.snapshot()} {
		if len(state.QueuedMessages) != 0 || state.SteeringMessage != nil || len(state.Turns) != 0 {
			t.Fatalf("%s after the cut: %s", name, truncateJSON(t, state))
		}
	}
	if ids := saved(); len(ids) != 0 {
		t.Fatalf("saved queue after the cut: %v", ids)
	}
	c.complete("two", "second question")
	if request := model.asked("second question"); strings.Contains(request, "mind the tests") || strings.Contains(request, "keep it short") {
		t.Fatalf("old steering reached the next turn: %s", request)
	}
}
