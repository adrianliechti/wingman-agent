package code

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

type forkingTestAgent struct {
	*uiTestAgent
	forkedFrom, forkTitle string
}

func (a *forkingTestAgent) ForkSession(_ context.Context, id, title string) (string, error) {
	a.forkedFrom, a.forkTitle = id, title
	return "forked-session", nil
}

func TestForkCommandSwitchesToForkedSession(t *testing.T) {
	testenv.WingmanHome(t)
	testenv.UserHome(t)
	a, base := newStreamTestApp([]harness.Message{{Role: harness.RoleUser, Content: []harness.Content{{Text: "hello"}}}})
	coder := &forkingTestAgent{uiTestAgent: base}
	a.agent = coder
	a.editor = NewEditor()
	a.turns = code.NewTurnManager(a.ctx, coder, a.handleTurnEvent)
	defer a.turns.Close()

	cmd := a.findBuiltin("/fork Try again")
	if cmd == nil || cmd.RunArgs == nil {
		t.Fatal("/fork is missing for a forking agent")
	}
	cmd.RunArgs(a, "Try again")
	waitForSessionOperation(t, a)

	if coder.forkedFrom != "session" || coder.forkTitle != "Try again" {
		t.Fatalf("forked %q with title %q", coder.forkedFrom, coder.forkTitle)
	}
	if a.sessionID != "forked-session" {
		t.Fatalf("current session = %q, want the fork", a.sessionID)
	}
	view := ansi.Strip(strings.Join(a.chatViewLines(100), "\n"))
	if !strings.Contains(view, "hello") || !strings.Contains(view, "Forked session") {
		t.Fatalf("fork view = %q", view)
	}
}

func TestForkCommandRequiresForkingAgent(t *testing.T) {
	a, _ := newStreamTestApp(nil)
	if a.findBuiltin("/fork") != nil {
		t.Fatal("/fork offered for an agent that cannot fork")
	}
}
