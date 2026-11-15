package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// truncateModel keeps what it was asked and can hold a reply back.
type truncateModel struct {
	mu       sync.Mutex
	requests []string
	hold     chan struct{} // while set, the replies to "slow" questions wait for it
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
	hold := m.hold
	m.mu.Unlock()
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
	fmt.Fprint(w, `data: {"type":"response.completed","sequence_number":2,"response":{"output":[{"type":"message","id":"answer","role":"assistant","status":"completed","content":[{"type":"output_text","text":"an answer","annotations":[]}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
}

// asked is the last request that carried a question.
func (m *truncateModel) asked(question string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if strings.Contains(m.requests[i], question) {
			return m.requests[i]
		}
	}
	return ""
}

type truncateChat struct {
	t      *testing.T
	client *ahp.Client
	other  *ahp.Client
	seq    int64
	state  *types.ChatState
	events *ahp.Subscription
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
	c := &truncateChat{t: t, client: ahpTestClient(t, endpoint, ""), other: ahpTestClient(t, endpoint, "")}
	for id, client := range map[string]*ahp.Client{"editor": c.client, "reader": c.other} {
		if _, err := client.Initialize(ctx, id, []string{"1.1.0"}, []string{types.RootResourceURI}); err != nil {
			t.Fatal(err)
		}
	}
	session := "ahp-session:/truncate"
	if err := c.client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: ahpPtr("wingman"), WorkingDirectories: []string{app.ahp.directoryURI()}}, nil); err != nil {
		t.Fatal(err)
	}
	c.state, c.events = ahpSubscribeChat(t, c.client, ahpChatURI(session))
	return model, c
}

func (c *truncateChat) dispatch(action types.StateAction) (echo types.ActionEnvelope, seen []types.ActionEnvelope) {
	c.t.Helper()
	c.seq++
	seq := c.seq
	if err := c.client.Notify(c.t.Context(), "dispatchAction", types.DispatchActionParams{Channel: c.state.Resource, ClientSeq: seq, Action: action}); err != nil {
		c.t.Fatal(err)
	}
	echo = ahpUntil(c.t, c.state, c.events, func(e types.ActionEnvelope) bool {
		seen = append(seen, e)
		return e.Origin != nil && e.Origin.ClientId == "editor" && e.Origin.ClientSeq == seq
	})
	return echo, seen
}

func (c *truncateChat) start(turn, text string) {
	c.t.Helper()
	c.dispatch(types.StateAction{Value: &types.ChatTurnStartedAction{Type: types.ActionTypeChatTurnStarted, TurnId: turn, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Message: types.Message{Text: text, Origin: types.MessageOrigin{Kind: types.MessageKindUser}}}})
}

func (c *truncateChat) complete(turn, text string) {
	c.t.Helper()
	c.start(turn, text)
	if !truncateTurns(c.state, turn) {
		ahpUntil(c.t, c.state, c.events, func(types.ActionEnvelope) bool { return truncateTurns(c.state, turn) })
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
	state, sub := ahpSubscribeChat(c.t, c.other, c.state.Resource)
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
	if echo, _ := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: ahpPtr("unknown")}}); echo.RejectionReason == nil {
		t.Fatal("a cut after an unknown turn was accepted")
	}
	kept := truncateJSON(t, c.state.Turns[:1])
	echo, seen := c.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: ahpPtr("one")}})
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
			if action, ok := event.(ahp.SubscriptionEventAction); ok && strings.Contains(truncateJSON(t, action.Envelope.Action), `"turnId":"two"`) {
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
