package code

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

type sideQuestionAgent struct {
	*uiTestAgent
	asked   chan string
	release chan struct{}
	err     error
}

func (a *sideQuestionAgent) Ask(ctx context.Context, _, question string, onText func(string)) (string, error) {
	a.asked <- question
	onText("Because ")
	select {
	case <-a.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if a.err != nil {
		return "", a.err
	}
	onText("the build needs it.")
	return "Because the build needs it.", nil
}

func newSideQuestionApp(t *testing.T) (*App, *sideQuestionAgent) {
	t.Helper()
	a, _ := newDiffPanelTestApp(t, 80, 14)
	agent := &sideQuestionAgent{uiTestAgent: a.agent.(*uiTestAgent), asked: make(chan string, 1), release: make(chan struct{})}
	a.agent = agent
	a.queue = make(chan func(), 8)
	return a, agent
}

func runPosted(t *testing.T, a *App) {
	t.Helper()
	select {
	case fn := <-a.queue:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("the side question did not finish")
	}
}

func screenOf(a *App) string {
	return ansi.Strip(strings.Join(a.overlay.Render(80, 14), "\n"))
}

func TestBtwAnswersInItsOwnPaneAndLeavesTheConversationAlone(t *testing.T) {
	a, agent := newSideQuestionApp(t)

	cmd := a.findBuiltin("/btw why that file?")
	if cmd == nil || !cmd.Busy || cmd.RunArgs == nil {
		t.Fatalf("/btw = %+v, want a command with arguments usable during a turn", cmd)
	}

	a.askAside("")
	if a.overlay != nil || !strings.Contains(ansi.Strip(strings.Join(a.chat, "\n")), "/btw <question>") {
		t.Fatalf("a question is required: overlay = %v chat = %q", a.overlay, a.chat)
	}
	chat := len(a.chat)

	a.askAside("why that file?")
	if got := <-agent.asked; got != "why that file?" {
		t.Fatalf("asked %q", got)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(screenOf(a), "Because"); {
		if time.Now().After(deadline) {
			t.Fatalf("the answer did not start to arrive:\n%s", screenOf(a))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if screen := screenOf(a); !strings.Contains(screen, "why that file?") || !strings.Contains(screen, "esc cancel") {
		t.Fatalf("pane while answering:\n%s", screen)
	}

	close(agent.release)
	runPosted(t, a)
	screen := screenOf(a)
	if !strings.Contains(screen, "Because the build needs it.") || !strings.Contains(screen, "i add to prompt") {
		t.Fatalf("pane with the answer:\n%s", screen)
	}
	if len(a.chat) != chat || len(a.agent.Messages(a.sessionID)) != 0 {
		t.Fatal("the side question entered the conversation")
	}

	if !a.overlay.HandleKey(inline.KeyEvent{Key: inline.KeyRune, Rune: 'i'}) {
		t.Fatal("i did not close the pane")
	}
	if text := a.editor.Text(); !strings.Contains(text, "why that file?") || !strings.Contains(text, "Because the build needs it.") {
		t.Fatalf("prompt = %q", text)
	}
}

func TestBtwCancelsWhenThePaneClosesAndReportsFailures(t *testing.T) {
	a, agent := newSideQuestionApp(t)

	a.askAside("first")
	<-agent.asked
	a.closeOverlay()
	runPosted(t, a)
	if a.overlay != nil {
		t.Fatal("a cancelled question reopened its pane")
	}

	agent.err = errors.New("model unavailable")
	a.askAside("second")
	<-agent.asked
	close(agent.release)
	runPosted(t, a)
	if screen := screenOf(a); !strings.Contains(screen, "Side question failed") || !strings.Contains(screen, "model unavailable") || strings.Contains(screen, "add to prompt") {
		t.Fatalf("pane after a failure:\n%s", screen)
	}
}
