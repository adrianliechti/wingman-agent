package code

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

type queueTestAgent struct {
	*uiTestAgent
	mu        sync.Mutex
	persisted corecode.TurnQueueState
	sent      chan string
}

func (a *queueTestAgent) LoadTurnQueue(string) (corecode.TurnQueueState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.persisted, nil
}
func (a *queueTestAgent) SaveTurnQueue(_ string, s corecode.TurnQueueState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.persisted = s
	return nil
}
func (a *queueTestAgent) Send(ctx context.Context, _ string, input []agent.Content) (iter.Seq2[agent.Message, error], error) {
	return func(func(agent.Message, error) bool) {
		select {
		case a.sent <- input[0].Text:
		case <-ctx.Done():
		}
	}, nil
}
func newQueueTestApp(t *testing.T, count int) (*App, *queueTestAgent) {
	t.Helper()
	app, fake := newStreamTestApp(nil)
	app.ctx = t.Context()
	app.editor = NewEditor()
	fakeQueue := &queueTestAgent{uiTestAgent: fake, sent: make(chan string, count+1)}
	for i := 0; i < count; i++ {
		fakeQueue.persisted.Inputs = append(fakeQueue.persisted.Inputs, corecode.TurnInput{ID: strings.Repeat("x", i+1), Content: []agent.Content{{Text: "recovered follow-up"}}})
	}
	app.agent = fakeQueue
	app.turns = corecode.NewTurnManager(app.ctx, fakeQueue, app.handleTurnEvent)
	app.drafts = newDraftStore(nil, "")
	t.Cleanup(app.turns.Close)
	app.syncQueuedInputs()
	return app, fakeQueue
}

func TestRecoveredQueueCanBeViewedEditedAndResumed(t *testing.T) {
	app, fake := newQueueTestApp(t, 1)
	visible := ansi.Strip(strings.Join(app.chatViewLines(100), "\n"))
	if !strings.Contains(visible, "recovered follow-up") || !app.queuePaused {
		t.Fatalf("recovered queue invisible: %q", visible)
	}
	app.showTurnQueue()
	if findPopupItem(app.popup, "resume") == nil {
		t.Fatal("missing resume action")
	}
	app.popup = nil
	app.editor.SetText("preserve my separate draft")
	app.runQueueCommand("edit x")
	app.editor.SetText("updated follow-up")
	app.submitInput()
	if app.editor.Text() != "preserve my separate draft" {
		t.Fatalf("editing queue lost separate draft: %q", app.editor.Text())
	}
	snapshot := app.turns.Snapshot("session")
	if !snapshot.Paused || len(snapshot.Inputs) != 1 || snapshot.Inputs[0].Input.Content[0].Text != "updated follow-up" {
		t.Fatalf("edited queue: %+v", snapshot)
	}
	app.runQueueCommand("resume")
	select {
	case text := <-fake.sent:
		if text != "updated follow-up" {
			t.Fatalf("sent %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not execute")
	}
}

func TestClearingLargeRecoveredQueueDoesNotBlockTheInputLoop(t *testing.T) {
	app, _ := newQueueTestApp(t, 200)
	done := make(chan struct{})
	go func() { app.runQueueCommand("clear"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queue events blocked the UI callback channel")
	}
	if snapshot := app.turns.Snapshot("session"); len(snapshot.Inputs) != 0 || snapshot.Paused {
		t.Fatalf("queue not cleared: %+v", snapshot)
	}
}

func TestEscapeKeepsDraftAndQueueEditCanBeCancelled(t *testing.T) {
	app, _ := newQueueTestApp(t, 1)
	app.editor.SetText("keep instructions")
	app.pendingFiles = []string{"main.go"}
	app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	if app.editor.Text() != "keep instructions" || len(app.pendingFiles) != 1 {
		t.Fatal("Escape discarded user work")
	}
	app.runQueueCommand("edit x")
	app.editor.SetText("unfinished queue edit")
	app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	if app.editor.Text() != "keep instructions" || app.editingQueueID != "" {
		t.Fatal("cancel did not restore prior draft")
	}
	if queuedText(app.turns.Snapshot("session").Inputs[0].Input) != "recovered follow-up" {
		t.Fatal("cancel changed queued input")
	}
}

func TestAltEnterQueuesAndShiftEnterInsertsNewline(t *testing.T) {
	app, _ := newQueueTestApp(t, 1)
	app.editor.SetText("later")
	app.handleKey(inline.KeyEvent{Key: inline.KeyEnter, Shift: true})
	if app.editor.Text() != "later\n" {
		t.Fatal("Shift+Enter should insert a newline")
	}
	app.handleKey(inline.KeyEvent{Key: inline.KeyEnter, Alt: true})
	snapshot := app.turns.Snapshot("session")
	if len(snapshot.Inputs) != 2 || snapshot.Inputs[1].Intent != corecode.TurnInputFollowUp {
		t.Fatalf("Alt+Enter did not queue: %+v", snapshot)
	}
}
