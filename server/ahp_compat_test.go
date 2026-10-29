package server

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
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
	a := h.attach(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
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
	a := h.attach(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
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
	a := h.attach(c, canonicalAHPURI("one", "saved"), "Test", time.Now())
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
