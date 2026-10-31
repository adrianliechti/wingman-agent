package session

import (
	"strconv"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func reviewSessionsJournal(t *testing.T, dir string, between ...harness.RuntimeEvent) {
	t.Helper()
	var events []harness.RuntimeEvent
	add := func(e harness.RuntimeEvent) {
		e.Sequence = uint64(len(events) + 1)
		e.ID = "event-" + strconv.FormatUint(e.Sequence, 10)
		e.At = time.Now().UTC()
		events = append(events, e)
	}
	for i, input := range []string{"a", "b"} {
		if i == 1 {
			for _, e := range between {
				add(e)
			}
		}
		add(harness.RuntimeEvent{Type: harness.EventTurnStarted, TurnID: "turn-" + input})
		add(harness.RuntimeEvent{Type: harness.EventMessage, Message: &harness.Message{Role: harness.RoleUser, InputID: input, Content: []harness.Content{{Text: "ask " + input}}}})
		add(harness.RuntimeEvent{Type: harness.EventMessage, Message: &harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "answer " + input}}}})
		add(harness.RuntimeEvent{Type: harness.EventTurnTerminal, TurnID: "turn-" + input, Terminal: &harness.RuntimeTerminal{Status: harness.RuntimeCompleted}})
	}
	if err := Save(dir, "s1", harness.State{Events: events}); err != nil {
		t.Fatal(err)
	}
}

// Bug: a fork through input "a" also copies a /compact checkpoint recorded after a's turn, so the fork's model context is the later summary instead of the history it was cut at.
func TestReviewSessionsForkThroughStopsBeforeALaterCompaction(t *testing.T) {
	dir := t.TempDir()
	reviewSessionsJournal(t, dir, harness.RuntimeEvent{Type: harness.EventContextCheckpoint, Context: []harness.Message{{Role: harness.RoleUser, Content: []harness.Content{{Text: "summary of a"}}}}, ContextReason: "compact model context"})
	if err := ForkThrough(dir, "s1", "s2", "", "a"); err != nil {
		t.Fatal(err)
	}
	forked, err := Load(dir, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if forked.State.ContextSet || len(forked.State.Messages) != 2 {
		t.Fatalf("fork through a carries a later checkpoint: context=%+v messages=%d", forked.State.Context, len(forked.State.Messages))
	}
}

func TestReviewSessionsForkThroughTheLastInputKeepsItsWholeTurn(t *testing.T) {
	dir := t.TempDir()
	reviewSessionsJournal(t, dir)
	if err := ForkThrough(dir, "s1", "s2", "", "b"); err != nil {
		t.Fatal(err)
	}
	forked, err := Load(dir, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.State.Messages) != 4 || forked.State.Messages[3].Content[0].Text != "answer b" {
		t.Fatalf("forked messages = %+v", forked.State.Messages)
	}
	source, err := Load(dir, "s1")
	if err != nil || len(source.State.Events) != len(forked.State.Events) {
		t.Fatalf("fork through the last input differs from its source: %v", err)
	}
	if err := ForkThrough(dir, "s1", "s3", "", ""); err == nil {
		t.Fatal("fork through an empty input id succeeded")
	}
	if err := ForkThrough(dir, "s1", "s1", "", "a"); err == nil {
		t.Fatal("fork through overwrote its source")
	}
}
