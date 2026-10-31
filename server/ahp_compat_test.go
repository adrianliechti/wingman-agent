package server

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestAHPCreationRetriesPreserveParametersAndErrors(test *testing.T) {
	test.Setenv("WINGMAN_HOME", test.TempDir())
	s, _, agent := newProtocolServer(test)
	h := s.ahpHost()
	create := func(params t.CreateSessionParams) error {
		raw, _ := json.Marshal(params)
		_, err := h.createSession(raw, "client")
		return err
	}
	checkCode := func(err error, code int) {
		test.Helper()
		var rpc *ahpRPCError
		if !errors.As(err, &rpc) || rpc.Code != code {
			test.Fatalf("want error %d, got %v", code, err)
		}
	}
	params := t.CreateSessionParams{Channel: "ahp-session:/retry", Provider: ahpPtr("one")}
	if err := create(params); err != nil {
		test.Fatal(err)
	}
	checkCode(create(params), -32003)
	params.Config = map[string]json.RawMessage{"model": json.RawMessage(`"other"`)}
	checkCode(create(params), -32602)
	if agent.created.Load() != 1 {
		test.Fatal("retry created a duplicate conversation")
	}

	params.Channel = "ahp-session:/invalid-config"
	params.Config = map[string]json.RawMessage{"model": json.RawMessage(`false`)}
	checkCode(create(params), -32602)
	// An incomplete creation must keep reporting its configuration error;
	// AlreadyExists would tell reconnecting clients it had succeeded.
	checkCode(create(params), -32602)
	if agent.created.Load() != 2 {
		test.Fatal("configuration retry created a duplicate conversation")
	}
}

func readAHPFrame(test *testing.T, client *ahpConnection) map[string]json.RawMessage {
	test.Helper()
	select {
	case data := <-client.writer.outbox:
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(data, &frame); err != nil {
			test.Fatal(err)
		}
		return frame
	case <-time.After(time.Second):
		test.Fatal("missing AHP frame")
		return nil
	}
}

func TestAHPSnapshotPrecedesConcurrentUpdates(test *testing.T) {
	s, b, _ := newProtocolServer(test)
	c := b.session("saved")
	c.load()
	h := s.ahpHost()
	a, _ := h.attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	entry := turnQueueEntry(code.TurnInput{ID: "turn", Content: []harness.Content{{Text: "hello"}}}, code.TurnInputActive, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &entry})
	for range 50 {
		peer := &ahpConnection{host: h, writer: newWSClient(nil), initialized: true, version: "1.1.0", subscriptions: map[string]bool{}}
		h.mu.Lock()
		h.clients[peer] = struct{}{}
		h.mu.Unlock()
		raw, _ := json.Marshal(t.SubscribeParams{Channel: a.chat.Resource})
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := peer.subscribeRequest(ahpRequest{ID: json.RawMessage("1"), Method: "subscribe", Params: raw}); err != nil {
				test.Error(err)
			}
		})
		wg.Go(func() { c.apply(Frame{Type: EvtTextDelta, ID: "answer", Text: "x"}) })
		wg.Wait()
		frame := readAHPFrame(test, peer)
		var result t.SubscribeResult
		if err := json.Unmarshal(frame["result"], &result); err != nil || result.Snapshot == nil {
			test.Fatalf("first frame must be snapshot: %s (%v)", frame, err)
		}
		for len(peer.writer.outbox) > 0 {
			update := readAHPFrame(test, peer)
			var action t.ActionEnvelope
			if err := json.Unmarshal(update["params"], &action); err != nil || action.ServerSeq <= result.Snapshot.FromSeq {
				test.Fatalf("update predates snapshot: %+v %v", action, err)
			}
		}
		h.mu.Lock()
		delete(h.clients, peer)
		h.mu.Unlock()
	}
}

func TestAHPReasoningTimingEndsAtNextPartAndSurvivesCompletion(test *testing.T) {
	s, b, _ := newProtocolServer(test)
	c := b.session("saved")
	c.load()
	h := s.ahpHost()
	a, _ := h.attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	reasoning := func(parts []t.ResponsePart) t.ReasoningResponsePart {
		test.Helper()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, part := range parts {
			if p, ok := part.Value.(*t.ReasoningResponsePart); ok {
				return *p
			}
		}
		test.Fatal("missing reasoning part")
		return t.ReasoningResponsePart{}
	}
	entry := turnQueueEntry(code.TurnInput{ID: "turn", Content: []harness.Content{{Text: "hello"}}}, code.TurnInputActive, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &entry})
	c.apply(Frame{Type: EvtReasoningDelta, ID: "think", Text: "Thinking"})
	time.Sleep(20 * time.Millisecond)
	c.apply(Frame{Type: EvtReasoningDelta, ID: "think", Text: " more"})
	if part := reasoning(a.chat.ActiveTurn.ResponseParts); part.StartedAt == nil || part.Duration != nil || part.Content != "Thinking more" {
		test.Fatalf("open reasoning run: %+v", part)
	}
	c.apply(Frame{Type: EvtTextDelta, ID: "answer", Text: "Done"})
	part := reasoning(a.chat.ActiveTurn.ResponseParts)
	if part.Duration == nil || *part.Duration < 20 {
		test.Fatalf("markdown must end the reasoning run: %+v", part)
	}
	done := turnQueueEntry(code.TurnInput{ID: "turn"}, code.TurnInputCompleted, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &done})
	if len(a.chat.Turns) != 1 || a.chat.ActiveTurn != nil {
		test.Fatalf("turn did not complete: %+v", a.chat)
	}
	if completed := reasoning(a.chat.Turns[0].ResponseParts); !reflect.DeepEqual(completed, part) {
		test.Fatalf("completion changed timing: %+v != %+v", completed, part)
	}
}

func TestAHPPendingMessagesPreserveKindsAndLegacySnapshots(test *testing.T) {
	s, b, _ := newProtocolServer(test)
	c := b.session("saved")
	c.load()
	h := s.ahpHost()
	a, _ := h.attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.PendingInputs = []TurnQueueEntry{{ID: "queued", Text: "queued", State: "queued", Intent: "follow_up"}, {ID: "steer-one", Text: "one", State: "queued", Intent: "steer"}, {ID: "steer-two", Text: "two", State: "queued", Intent: "steer"}}
	c.publishStateLocked()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(a.chat.QueuedMessages) != 3 {
		test.Fatalf("lost mixed messages: %+v", a.chat.QueuedMessages)
	}
	if *a.chat.QueuedMessages[1].Kind != t.PendingMessageKindSteering || a.chat.SteeringMessage.Id != "steer-two" {
		test.Fatal("incorrect steering state")
	}
	legacy := (&ahpConnection{version: "1.0.0"}).chatSnapshot(&a.chat)
	if len(legacy.QueuedMessages) != 1 || legacy.SteeringMessage.Id != "steer-two" {
		test.Fatalf("legacy snapshot duplicates steering: %+v", legacy)
	}
}

func TestAHPFreeformAnswerReachesItsCompanionField(test *testing.T) {
	view := PromptView{ID: "ask", Kind: PromptKindAsk, Fields: []tool.ElicitField{
		{Name: "choice", Enum: []string{"a", "b"}, Strict: true, Default: "b"},
		{Name: "other", Type: "string", CustomAnswerFor: "choice"},
	}}
	request := ahpPromptPart(&ahpPrompt{view: view}).Value.(*t.InputRequestResponsePart).Request
	question, ok := request.Questions[0].Value.(*t.ChatInputSingleSelectQuestion)
	if len(request.Questions) != 1 || !ok || !*question.AllowFreeformInput || question.Options[1].Recommended == nil {
		test.Fatalf("questions = %+v", request.Questions)
	}
	answer := func(value string, freeform ...string) map[string]t.ChatInputAnswer {
		return map[string]t.ChatInputAnswer{"choice": {Value: &t.ChatInputAnswered{State: t.ChatInputAnswerStateSubmitted, Value: t.ChatInputAnswerValue{Value: &t.ChatInputSelectedAnswerValue{Kind: t.ChatInputAnswerValueKindSelected, Value: value, FreeformValues: freeform}}}}}
	}
	if content, err := ahpAnswers(view, answer("", "custom")); err != nil || content["other"] != "custom" || content["choice"] != nil {
		test.Fatalf("free-form answer = %v, %v", content, err)
	}
	if content, err := ahpAnswers(view, answer("a")); err != nil || content["choice"] != "a" || content["other"] != nil {
		test.Fatalf("selected answer = %v, %v", content, err)
	}
}

// The host's reduced state must match its own projection, or every sync would
// truncate and replay the active turn.
func TestAHPToolCallsReduceToTheirProjection(test *testing.T) {
	s, b, _ := newProtocolServer(test)
	c := b.session("saved")
	c.load()
	a, _ := s.ahpHost().attachSession(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
	check := func(step string) {
		test.Helper()
		c.mu.Lock()
		defer c.mu.Unlock()
		a.host.mu.Lock()
		defer a.host.mu.Unlock()
		_, active := a.transcript(c)
		if active == nil || a.chat.ActiveTurn == nil || !reflect.DeepEqual(active.ResponseParts, a.chat.ActiveTurn.ResponseParts) {
			test.Fatalf("%s: reduced state diverged\n%+v\n%+v", step, active, a.chat.ActiveTurn)
		}
	}
	entry := turnQueueEntry(code.TurnInput{ID: "turn", Content: []harness.Content{{Text: "hello"}}}, code.TurnInputActive, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &entry})
	locations := []harness.ToolLocation{{Path: "main.go", Line: 3}}
	c.apply(Frame{Type: EvtToolCall, ID: "edit", Name: "edit", Kind: "edit", Args: `{"pa`, Hint: "main.go", Partial: true})
	check("streaming")
	c.apply(Frame{Type: EvtToolCall, ID: "edit", Name: "edit", Kind: "edit", Args: `{"path":"main.go"}`, Hint: "main.go", Locations: locations})
	check("running")
	c.apply(Frame{Type: EvtToolCall, ID: "edit", Name: "edit", Kind: "edit", Args: `{"path":"main.go"}`, Hint: "main.go", Locations: append(locations, harness.ToolLocation{Path: "util.go"})})
	check("running with new locations")
	c.apply(Frame{Type: EvtToolResult, ID: "edit", Name: "edit", Kind: "edit", Args: `{"path":"main.go"}`, Hint: "main.go", Locations: locations, Content: "done"})
	check("completed")
	c.apply(Frame{Type: EvtToolCall, ID: "run", Name: "shell", Kind: "execute", Args: `{"command":"ls"}`})
	check("terminal tool")
	h := a.host
	h.mu.Lock()
	defer h.mu.Unlock()
	parts := a.chat.ActiveTurn.ResponseParts
	edit := parts[0].Value.(*t.ToolCallResponsePart).ToolCall.Value.(*t.ToolCallCompletedState)
	run := parts[1].Value.(*t.ToolCallResponsePart).ToolCall.Value.(*t.ToolCallRunningState)
	if *edit.Intention != "main.go" || string(edit.Meta["toolKind"]) != `"edit"` || len(edit.Content) != 2 || string(run.Meta["toolKind"]) != `"terminal"` {
		test.Fatalf("tool presentation = %+v %+v", edit, run)
	}
	if resource, ok := edit.Content[1].Value.(*t.ToolResultResourceContent); !ok || !strings.HasSuffix(string(resource.Uri), "/main.go#L3") {
		test.Fatalf("location = %+v", edit.Content[1].Value)
	}
}
