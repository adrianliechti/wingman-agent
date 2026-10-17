package code

import (
	"context"
	"testing"

	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestCtrlJInsertsNewline(t *testing.T) {
	a := &App{editor: NewEditor()}
	a.editor.SetText("first line")

	a.handleKey(inline.KeyEvent{Key: inline.KeyCtrl, Rune: 'j'})

	if got := a.editor.Text(); got != "first line\n" {
		t.Fatalf("editor text = %q, want newline without submission", got)
	}
}

func TestTabTogglesPlanAndAgent(t *testing.T) {
	agent := newUITestAgent(nil)
	a := &App{ctx: context.Background(), queue: make(chan func(), 64), agent: agent, editor: NewEditor()}

	a.handleKey(inline.KeyEvent{Key: inline.KeyTab})
	waitForSessionOperation(t, a)
	if agent.mode != "plan" {
		t.Fatalf("Tab mode = %q, want plan", agent.mode)
	}
	a.handleKey(inline.KeyEvent{Key: inline.KeyTab})
	waitForSessionOperation(t, a)
	if agent.mode != "agent" {
		t.Fatalf("second Tab mode = %q, want agent", agent.mode)
	}
}

func TestBacktabTogglesUnattendedAndAgent(t *testing.T) {
	agent := newUITestAgent(nil)
	a := &App{ctx: context.Background(), queue: make(chan func(), 64), agent: agent, editor: NewEditor()}

	a.handleKey(inline.KeyEvent{Key: inline.KeyBacktab})
	waitForSessionOperation(t, a)
	if agent.mode != "unattended" {
		t.Fatalf("Shift+Tab mode = %q, want unattended", agent.mode)
	}
	a.handleKey(inline.KeyEvent{Key: inline.KeyBacktab})
	waitForSessionOperation(t, a)
	if agent.mode != "agent" {
		t.Fatalf("second Shift+Tab mode = %q, want agent", agent.mode)
	}
}

func TestModeTogglesDuringRunningTurn(t *testing.T) {
	agent := newUITestAgent(nil)
	a := &App{ctx: context.Background(), queue: make(chan func(), 64), agent: agent, editor: NewEditor()}
	a.phase.Store(int32(PhaseToolRunning))

	a.handleKey(inline.KeyEvent{Key: inline.KeyBacktab})
	waitForSessionOperation(t, a)
	if agent.mode != "unattended" {
		t.Fatalf("Shift+Tab mode while streaming = %q, want unattended", agent.mode)
	}
	a.handleKey(inline.KeyEvent{Key: inline.KeyTab})
	waitForSessionOperation(t, a)
	if agent.mode != "plan" {
		t.Fatalf("Tab mode while streaming = %q, want plan", agent.mode)
	}
}

type unattendedUITestAgent struct {
	*uiTestAgent
	unattended bool
}

func (a *unattendedUITestAgent) Unattended(string) bool { return a.unattended }
func (a *unattendedUITestAgent) SetUnattended(_ context.Context, _ string, enabled bool) error {
	a.unattended = enabled
	return nil
}
func (a *unattendedUITestAgent) Modes(string) ([]corecode.Mode, string) {
	return []corecode.Mode{{ID: "agent", Name: "Agent"}, {ID: "plan", Name: "Plan"}}, a.mode
}

func TestBacktabPreservesPlanWithIndependentUnattendedPolicy(t *testing.T) {
	agent := &unattendedUITestAgent{uiTestAgent: newUITestAgent(nil)}
	agent.mode = "plan"
	a := &App{ctx: context.Background(), queue: make(chan func(), 64), agent: agent, editor: NewEditor()}
	for _, want := range []bool{true, false} {
		a.handleKey(inline.KeyEvent{Key: inline.KeyBacktab})
		waitForSessionOperation(t, a)
		if agent.mode != "plan" || a.unattended() != want {
			t.Fatalf("Shift+Tab changed Plan: mode=%s unattended=%v", agent.mode, a.unattended())
		}
	}
	found := false
	for _, command := range a.builtinCommands() {
		if command.Name == "/unattended" {
			found = true
		}
	}
	if !found {
		t.Fatal("independent unattended command is missing")
	}
}
