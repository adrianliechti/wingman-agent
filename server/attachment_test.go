package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func attachedChat(chat string, end *string) types.MessageAttachment {
	return types.MessageAttachment{Value: &types.MessageChatAttachment{Type: types.MessageAttachmentKindChat, Resource: chat, Label: "Earlier chat", EndTurn: end}}
}

// truncateInputs are the texts of the messages a model request carried, in order.
func truncateInputs(t *testing.T, request string) (texts []string) {
	t.Helper()
	var body struct {
		Input []struct {
			Role    string
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal([]byte(request), &body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Input {
		var text strings.Builder
		for _, part := range item.Content {
			text.WriteString(part.Text)
		}
		if item.Role != "" {
			texts = append(texts, text.String())
		}
	}
	return texts
}

func TestAHPChatAttachmentSuppliesAPinnedTranscript(t *testing.T) {
	model, target := truncateTest(t)
	source := target.open("ahp-session:/source")
	empty := target.open("ahp-session:/empty")
	source.complete("s1", "the heron flies at dawn")
	source.complete("s2", "the otter swims at dusk")

	// Nothing starts for a chat or a turn that cannot be pinned.
	model.mu.Lock()
	model.hold = make(chan struct{})
	model.mu.Unlock()
	source.start("running", "slow question")
	for name, attachment := range map[string]types.MessageAttachment{
		"unknown chat": attachedChat("ahp-chat:/missing", nil),
		"unknown turn": attachedChat(source.state.Resource, new("missing")),
		"running turn": attachedChat(source.state.Resource, new("running")),
	} {
		if echo := target.start("rejected", "anything", attachment); echo.RejectionReason == nil || target.state.ActiveTurn != nil || len(target.state.Turns) != 0 {
			t.Fatalf("%s: %+v left %s", name, echo, truncateJSON(t, target.state))
		}
	}
	close(model.hold)
	until(t, source.state, source.events, func(types.ActionEnvelope) bool { return truncateTurns(source.state, "running") })

	target.complete("t1", "what flies?", attachedChat(source.state.Resource, new("s1")))
	inputs := truncateInputs(t, model.asked("what flies?"))
	question := len(inputs) - 1
	if question < 1 || inputs[question] != "what flies?" {
		t.Fatalf("the question is not a message of its own: %q", inputs)
	}
	// The transcript precedes the message, through the turn the attachment names.
	transcript := inputs[question-1]
	if !strings.HasPrefix(transcript, `<attached_chat label="Earlier chat"`) || !strings.Contains(transcript, "\nthe heron flies at dawn\n") || !strings.Contains(transcript, "an answer") || strings.Contains(transcript, "otter") {
		t.Fatalf("transcript: %q", transcript)
	}
	if len(target.state.Turns) != 1 || len(target.state.Turns[0].Message.Attachments) != 1 {
		t.Fatalf("the transcript shows in the chat: %s", truncateJSON(t, target.state.Turns))
	}

	// What the source says later is not part of what was attached; a chat without turns attaches nothing.
	source.complete("s3", "the wren sings at noon")
	target.complete("t2", "and now?", attachedChat(empty.state.Resource, nil))
	request := model.asked("and now?")
	if strings.Count(request, "the heron flies at dawn") != 1 || strings.Contains(request, "wren") || strings.Contains(request, "otter") || strings.Contains(request, "slow question") {
		t.Fatalf("the model was asked: %s", request)
	}

	// The transcript leaves the history with the turn it came with.
	if echo, _ := target.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated, TurnId: new("t1")}}); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	target.complete("t3", "still there?")
	if request := model.asked("still there?"); strings.Count(request, "the heron flies at dawn") != 1 {
		t.Fatalf("the cut dropped the transcript of a turn it kept: %s", request)
	}
	if echo, _ := target.dispatch(types.StateAction{Value: &types.ChatTruncatedAction{Type: types.ActionTypeChatTruncated}}); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	target.complete("t4", "anything left?")
	if request := model.asked("anything left?"); strings.Contains(request, "heron") {
		t.Fatalf("the cut kept the transcript of a turn it dropped: %s", request)
	}
}

func TestAHPForkKeepsItsSourceTurnsAsTheyWere(t *testing.T) {
	_, c := truncateTest(t)
	c.complete("one", "first question")
	c.complete("two", "second question")
	session := strings.Replace(c.state.Resource, "ahp-chat:", "ahp-session:", 1)
	for through, kept := range map[string]int{"two": 2, "one": 1} {
		fork := "ahp-chat:/fork-" + through
		source := types.ChatSource{Value: &types.ForkChatSource{Kind: types.ChatSourceKindFork, Chat: c.state.Resource, TurnId: through}}
		if err := c.client.Request(t.Context(), "createChat", types.CreateChatParams{Channel: session, Chat: fork, Source: &source}, nil); err != nil {
			t.Fatal(err)
		}
		state, sub := subscribeChat(t, c.other, fork)
		sub.Close()
		// Timing, usage and the messages as sent exist only in the source chat.
		if want, got := truncateJSON(t, c.state.Turns[:kept]), truncateJSON(t, state.Turns); want != got || state.Turns[0].StartedAt == nil || state.Turns[0].Usage == nil {
			t.Fatalf("fork through %s:\n%s\n%s", through, got, want)
		}
	}
}

// The host shows a queued message, and the turn that consumes it, as the client sent it.
func TestAHPQueuedMessageKeepsItsAttachments(t *testing.T) {
	model, target := truncateTest(t)
	source := target.open("ahp-session:/source")
	source.complete("s1", "the heron flies at dawn")
	model.mu.Lock()
	model.hold = make(chan struct{})
	model.mu.Unlock()
	target.start("busy", "slow question")
	for model.asked("slow question") == "" {
		time.Sleep(5 * time.Millisecond)
	}
	message := types.Message{Text: "what flies?", Origin: types.MessageOrigin{Kind: types.MessageKindUser}, Attachments: []types.MessageAttachment{attachedChat(source.state.Resource, new("s1"))}}
	if echo, _ := target.dispatch(types.StateAction{Value: &types.ChatPendingMessageSetAction{Type: types.ActionTypeChatPendingMessageSet, Kind: types.PendingMessageKindQueued, Id: "later", Message: message}}); echo.RejectionReason != nil {
		t.Fatal(*echo.RejectionReason)
	}
	sent := truncateJSON(t, message)
	for name, state := range map[string]*types.ChatState{"mirror": target.state, "snapshot": target.snapshot()} {
		if len(state.QueuedMessages) != 1 || truncateJSON(t, state.QueuedMessages[0].Message) != sent {
			t.Fatalf("%s queue: %s", name, truncateJSON(t, state.QueuedMessages))
		}
	}
	close(model.hold)
	until(t, target.state, target.events, func(types.ActionEnvelope) bool { return truncateTurns(target.state, "busy", "later") })
	for name, state := range map[string]*types.ChatState{"mirror": target.state, "snapshot": target.snapshot()} {
		if len(state.QueuedMessages) != 0 || truncateJSON(t, state.Turns[len(state.Turns)-1].Message) != sent {
			t.Fatalf("%s turns: %s", name, truncateJSON(t, state))
		}
	}
	if request := model.asked("what flies?"); !strings.Contains(request, "the heron flies at dawn") {
		t.Fatalf("the model was asked: %s", request)
	}
}
