package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
)

type protocolAgent struct {
	code.Agent
	mu       sync.Mutex
	messages []harness.Message
	queue    code.TurnQueueState
	model    string
	failSave bool
	created  atomic.Int32
	sent     atomic.Int32
	loaded   atomic.Int32
	gate     chan struct{}
	creating chan struct{} // when set, NewSession waits for it to close
}

func (a *protocolAgent) Name() string { return "test" }
func (a *protocolAgent) Models(string) ([]model.Model, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return nil, a.model
}
func (a *protocolAgent) Effort(string) (string, []string)   { return "auto", []string{"auto", "high"} }
func (a *protocolAgent) Modes(string) ([]code.Mode, string) { return nil, "" }
func (a *protocolAgent) Usage(string) harness.Usage         { return harness.Usage{} }
func (a *protocolAgent) SetModel(_ context.Context, _, value string) error {
	a.mu.Lock()
	a.model = value
	a.mu.Unlock()
	return nil
}
func (a *protocolAgent) NewSession(context.Context) (string, error) {
	if a.creating != nil {
		<-a.creating
	}
	return fmt.Sprintf("session-%d", a.created.Add(1)), nil
}
func (a *protocolAgent) LoadSession(_ context.Context, id string) error {
	a.loaded.Add(1)
	if id == "missing" {
		return errors.New("session not found")
	}
	return nil
}
func (a *protocolAgent) DeleteSession(context.Context, string) error              { return nil }
func (a *protocolAgent) ListSessions(context.Context) ([]code.SessionInfo, error) { return nil, nil }
func (a *protocolAgent) Messages(string) []harness.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]harness.Message(nil), a.messages...)
}
func (a *protocolAgent) LoadTurnQueue(string) (code.TurnQueueState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queue, nil
}
func (a *protocolAgent) SaveTurnQueue(_ string, queue code.TurnQueueState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failSave {
		return errors.New("disk full")
	}
	a.queue = queue
	return nil
}
func (a *protocolAgent) Cancel(string) {}
func (a *protocolAgent) Close() error  { return nil }
func (a *protocolAgent) Send(ctx context.Context, _ string, input []harness.Content) (iter.Seq2[harness.Message, error], error) {
	a.sent.Add(1)
	a.mu.Lock()
	a.messages = append(a.messages, harness.Message{Role: harness.RoleUser, InputID: harness.InputIDFromContext(ctx), Content: harness.CloneContent(input)})
	a.mu.Unlock()
	return func(yield func(harness.Message, error) bool) {
		if !yield(harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "live prefix", TextID: "answer"}}}, nil) {
			return
		}
		if a.gate != nil {
			select {
			case <-a.gate:
			case <-ctx.Done():
				yield(harness.Message{}, ctx.Err())
				return
			}
		}
		a.mu.Lock()
		a.messages = append(a.messages, harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "live prefix", TextID: "answer"}}})
		a.mu.Unlock()
	}, nil
}

func newProtocolServer(t *testing.T) (*Server, *backendRuntime, *protocolAgent) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &Server{ctx: ctx, cancel: cancel, scope: WorkspaceScope{"workspace", "instance"}, backends: map[string]*backendEntry{}}
	a := &protocolAgent{}
	b := s.bindBackend("one", a)
	s.backends[b.id] = &backendEntry{runtime: b}
	s.mux = chi.NewRouter()
	s.mux.Route("/api", s.registerSessionRoutes)
	s.mux.Get("/ahp", s.handleAHP)
	s.handler = s.checkInstance(s.mux)
	t.Cleanup(s.Close)
	return s, b, a
}

// Controller tests address domain operations directly. Wire behavior is covered
// separately by the official TypeScript client and AHP WebSocket tests.
func protocolRequest(t *testing.T, s *Server, path string, command Command) *httptest.ResponseRecorder {
	t.Helper()
	parts := strings.Split(strings.Trim(path, "/"), "/")
	b, err := s.backend(parts[3])
	if err != nil {
		t.Fatal(err)
	}
	var record commandReceipt
	if len(parts) == 5 {
		record = b.createSession(command)
	} else {
		if !b.beginOperation() {
			record = commandReceipt{status: 409, message: "workspace closed"}
		} else {
			defer b.operations.Done()
			record = b.session(parts[5]).command(t.Context(), command)
		}
	}
	rec := httptest.NewRecorder()
	if record.status != 0 {
		http.Error(rec, record.message, record.status)
	} else {
		writeJSON(rec, record.receipt)
	}
	return rec
}

func waitProtocol(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionRestoresEditableQueueAndRejectsMissingHistory(t *testing.T) {
	s, b, a := newProtocolServer(t)
	a.queue = code.TurnQueueState{Inputs: []code.TurnInput{{ID: "saved-input", Intent: code.TurnInputFollowUp, Content: []harness.Content{{Text: "original"}, {Text: "[File: main.go]"}, {Text: "secret", Hidden: true}}}}}
	c := b.session("saved")
	c.load()
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if len(state.PendingInputs) != 1 || state.PendingInputs[0].ID != "saved-input" || state.PendingInputs[0].Text != "original" || !b.turns.Snapshot("saved").Paused {
		t.Fatalf("restored queue: %+v", state)
	}
	rec := protocolRequest(t, s, "/api/v2/backends/one/sessions/saved/commands", Command{ID: "edit", Epoch: c.epoch, Type: "queue_update", InputID: "saved-input", Text: "edited", Files: []string{"next.go"}})
	if rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	restored := b.turns.Snapshot("saved").Inputs[0].Input
	if restored.Display == nil || restored.Display.Text != "edited" || restored.Display.Files[0] != "next.go" {
		t.Fatalf("input: %+v", restored)
	}
	missing := b.session("missing")
	missing.load()
	missing.mu.Lock()
	defer missing.mu.Unlock()
	if missing.state.Status != "error" || missing.state.Error == nil {
		t.Fatalf("missing history was accepted: %+v", missing.state)
	}
}

func TestSessionReceiptsDeduplicateAfterCompletionAndCreation(t *testing.T) {
	s, b, a := newProtocolServer(t)
	var created Receipt
	for range 2 {
		rec := protocolRequest(t, s, "/api/v2/backends/one/sessions", Command{ID: "draft-request", Type: "create"})
		if rec.Code != 200 {
			t.Fatalf("create: %s", rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
	}
	if a.created.Load() != 1 {
		t.Fatalf("created %d sessions", a.created.Load())
	}
	path := "/api/v2/backends/one/sessions/" + created.Ref.SessionID + "/commands"
	command := Command{ID: "send-request", Epoch: created.Epoch, Type: "send", Text: "hello"}
	rec := protocolRequest(t, s, path, command)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	waitProtocol(t, func() bool { return a.sent.Load() == 1 && !snapshotHasActive(b.turns.Snapshot(created.Ref.SessionID)) })
	rec = protocolRequest(t, s, path, command)
	if rec.Code != 200 || a.sent.Load() != 1 {
		t.Fatalf("retry executed again: %d sends %d", rec.Code, a.sent.Load())
	}
	command.Text = "changed"
	if rec := protocolRequest(t, s, path, command); rec.Code != 409 {
		t.Fatalf("changed request accepted: %d", rec.Code)
	}
	command.ID = "new"
	command.Epoch = "old-epoch"
	if rec := protocolRequest(t, s, path, command); rec.Code != 409 {
		t.Fatalf("old epoch accepted: %d", rec.Code)
	}
}

func TestSessionPromptResponsesRemainAvailableDuringLoadAndHaveOneWinner(t *testing.T) {
	s, b, _ := newProtocolServer(t)
	c := b.session("saved")
	c.opMu.Lock() // A backend load/settings operation is waiting for UI.
	defer c.opMu.Unlock()
	ctx, cancel := context.WithCancel(code.WithSessionID(b.ctx, "saved"))
	defer cancel()
	done := make(chan bool, 1)
	go func() { approved, _ := b.Confirm(ctx, "Proceed?"); done <- approved }()
	waitProtocol(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.prompts) == 1 })
	c.mu.Lock()
	id := c.state.Prompts[0].ID
	c.mu.Unlock()
	path := "/api/v2/backends/one/sessions/saved/commands"
	command := Command{ID: "answer", Epoch: c.epoch, Type: "prompt_response", PromptID: id, Action: "accept"}
	rec := protocolRequest(t, s, path, command)
	if rec.Code != 200 {
		t.Fatalf("response: %d %s", rec.Code, rec.Body)
	}
	select {
	case approved := <-done:
		if !approved {
			t.Fatal("not approved")
		}
	case <-time.After(time.Second):
		t.Fatal("prompt response blocked behind load")
	}
	command.ID = "other-client"
	command.Action = "decline"
	if rec := protocolRequest(t, s, path, command); rec.Code != 409 {
		t.Fatal("second response won")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.state.Prompts) != 0 {
		t.Fatal("resolved prompt survived reconnect")
	}
}

func TestSessionBackendsOwnIdenticalNativeIDsAndDeletionIsTerminal(t *testing.T) {
	s, one, a := newProtocolServer(t)
	otherAgent := &protocolAgent{}
	two := s.bindBackend("two", otherAgent)
	s.backends["two"] = &backendEntry{runtime: two}
	c := one.session("same")
	c.load()
	other := two.session("same")
	other.load()
	if c.ref == other.ref || c.epoch == other.epoch {
		t.Fatal("session identity collided")
	}
	value := "model-b"
	rec := protocolRequest(t, s, "/api/v2/backends/two/sessions/same/commands", Command{ID: "settings", Epoch: other.epoch, Type: "settings", Model: &value})
	if rec.Code != 200 {
		t.Fatalf("settings: %s", rec.Body)
	}
	if _, model := a.Models("same"); model != "" {
		t.Fatal("settings crossed backends")
	}
	rec = protocolRequest(t, s, "/api/v2/backends/one/sessions/same/commands", Command{ID: "delete", Epoch: c.epoch, Type: "delete"})
	if rec.Code != 200 {
		t.Fatalf("delete: %s", rec.Body)
	}
	c.apply(Frame{Type: EvtTextDelta, Text: "late"})
	rec = protocolRequest(t, s, "/api/v2/backends/one/sessions/same/commands", Command{ID: "late", Epoch: c.epoch, Type: "send", Text: "late"})
	if rec.Code == 200 || a.sent.Load() != 0 {
		t.Fatal("deleted session accepted input")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Status != "deleted" || len(c.entries) > 0 {
		t.Fatal("late callback resurrected deletion")
	}
}

type unattendedProtocolAgent struct {
	*protocolAgent
	unattended bool
}

func (a *unattendedProtocolAgent) Modes(string) ([]code.Mode, string) {
	return []code.Mode{{ID: code.AgentModeID}, {ID: code.PlanModeID}}, code.PlanModeID
}
func (a *unattendedProtocolAgent) Unattended(string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unattended
}
func (a *unattendedProtocolAgent) SetUnattended(_ context.Context, _ string, enabled bool) error {
	a.mu.Lock()
	a.unattended = enabled
	a.mu.Unlock()
	return nil
}

func TestSessionSettingsRoundTripIndependentUnattendedPolicy(t *testing.T) {
	s, other, _ := newProtocolServer(t)
	a := &unattendedProtocolAgent{protocolAgent: &protocolAgent{model: "selected-model"}}
	b := s.bindBackend("policy", a)
	s.backends["policy"] = &backendEntry{runtime: b}
	c := b.session("saved")
	c.load()
	for _, enabled := range []bool{true, false} {
		rec := protocolRequest(t, s, "/api/v2/backends/policy/sessions/saved/commands", Command{
			ID: fmt.Sprintf("unattended-%t", enabled), Epoch: c.epoch, Type: "settings", Unattended: &enabled,
		})
		if rec.Code != 200 {
			t.Fatalf("settings: %d %s", rec.Code, rec.Body)
		}
		settings := b.settings("saved")
		if settings.Unattended == nil || *settings.Unattended != enabled || settings.Mode != code.PlanModeID || settings.Model != "selected-model" {
			t.Fatalf("policy changed mode/model or did not round-trip: %+v", settings)
		}
	}
	if other.settings("saved").Unattended != nil {
		t.Fatal("provider without independent unattended support advertised it")
	}
}

func TestSessionQueuePersistenceFailureDoesNotAcknowledgeClearing(t *testing.T) {
	s, b, a := newProtocolServer(t)
	a.queue = code.TurnQueueState{Inputs: []code.TurnInput{{ID: "queued", Content: []harness.Content{{Text: "keep me"}}}}}
	c := b.session("saved")
	c.load()
	a.mu.Lock()
	a.failSave = true
	a.mu.Unlock()
	rec := protocolRequest(t, s, "/api/v2/backends/one/sessions/saved/commands", Command{ID: "cancel", Epoch: c.epoch, Type: "cancel", ClearQueue: true})
	if rec.Code != 409 {
		t.Fatalf("cancel silently succeeded: %d", rec.Code)
	}
	if len(b.turns.Snapshot("saved").Inputs) != 1 {
		t.Fatal("failed persistence removed the input")
	}
}

func TestAHPWebSocketClosesWithWorkspace(t *testing.T) {
	s, _, _ := newProtocolServer(t)
	web := httptest.NewServer(s)
	defer web.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+web.URL[4:]+"/ahp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"channel":"ahp-root://","clientId":"close-test","protocolVersions":["1.0.0"]}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		t.Fatal("workspace close did not close AHP")
	}
}

func TestSessionEntryIdentitySurvivesCommitAndLateSteering(t *testing.T) {
	_, b, a := newProtocolServer(t)
	c := b.session("saved")
	c.load()
	input := code.TurnInput{ID: "input", Content: []harness.Content{{Text: "guidance"}}}
	entry := turnQueueEntry(input, code.TurnInputSteered, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &entry})
	c.apply(Frame{Type: EvtTextDelta, ID: "text", Text: "answer"})
	c.mu.Lock()
	userID, textID := c.entries[0].ID, c.entries[1].ID
	c.mu.Unlock()
	a.mu.Lock()
	a.messages = []harness.Message{{Role: harness.RoleUser, InputID: "input", Content: input.Content}, {Role: harness.RoleAssistant, Content: []harness.Content{{Text: "answer", TextID: "text"}}}}
	a.mu.Unlock()
	c.replaceHistory()
	c.apply(Frame{Type: EvtTurnInput, Input: &entry}) // accepted steer notification crossed the commit boundary
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 2 || c.entries[0].ID != userID || c.entries[1].ID != textID {
		t.Fatalf("commit changed identity or duplicated steering: %+v", c.entries)
	}
}

type compactingProtocolAgent struct {
	*protocolAgent
	focus   chan string
	release chan error
}

func (a *compactingProtocolAgent) Compact(ctx context.Context, _, focus string) (harness.CompactResult, error) {
	a.focus <- focus
	select {
	case err := <-a.release:
		if err != nil {
			return harness.CompactResult{}, err
		}
		return harness.CompactResult{TokensBefore: 900, TokensAfter: 90}, nil
	case <-ctx.Done():
		return harness.CompactResult{}, ctx.Err()
	}
}

func TestSessionCompactMessageRunsAsTurn(t *testing.T) {
	s, other, _ := newProtocolServer(t)
	a := &compactingProtocolAgent{protocolAgent: &protocolAgent{}, focus: make(chan string, 1), release: make(chan error, 1)}
	b := s.bindBackend("compact", a)
	s.backends["compact"] = &backendEntry{runtime: b}
	c := b.session("saved")
	c.load()
	if !b.settings("saved").CanCompact || other.settings("saved").CanCompact {
		t.Fatal("compaction capability not advertised per backend")
	}
	state := func() SessionState {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.state
	}

	entries := func() []TranscriptEntry {
		c.mu.Lock()
		defer c.mu.Unlock()
		return slices.Clone(c.entries)
	}

	rec := protocolRequest(t, s, "/api/v2/backends/compact/sessions/saved/commands", Command{ID: "compact-1", Epoch: c.epoch, Type: "send", Text: "/compact api design"})
	if rec.Code != 200 {
		t.Fatalf("compact: %d %s", rec.Code, rec.Body)
	}
	if focus := <-a.focus; focus != "api design" {
		t.Fatalf("focus=%q", focus)
	}
	waitProtocol(t, func() bool { return state().Phase == "compacting" })
	a.release <- nil
	waitProtocol(t, func() bool { return state().Phase == "idle" && len(entries()) == 2 })
	// The agent's history has no record of the compaction; the turn survives its reload.
	c.replaceHistory()
	if e := entries(); len(e) != 2 || e[0].ID != "input:compact-1" || e[0].Content != "/compact api design" || e[1].Type != "notice" || e[1].Content != "Context compacted · ~900 → ~90 tokens" || state().Error != nil {
		t.Fatalf("compaction turn = %+v", e)
	}

	rec = protocolRequest(t, s, "/api/v2/backends/compact/sessions/saved/commands", Command{ID: "compact-2", Epoch: c.epoch, Type: "send", Text: "/compact"})
	if rec.Code != 200 {
		t.Fatalf("compact: %d %s", rec.Code, rec.Body)
	}
	if focus := <-a.focus; focus != "" {
		t.Fatalf("focus=%q", focus)
	}
	a.release <- errors.New("compaction did not reduce context")
	waitProtocol(t, func() bool { return state().Phase == "idle" && state().Error != nil })
	if st, e := state(), entries(); *st.Error != "compaction did not reduce context" || len(e) != 3 || e[2].ID != "input:compact-2" {
		t.Fatalf("state after failure = %+v %+v", st, e)
	}
}
