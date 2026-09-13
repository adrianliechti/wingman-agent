package code

import (
	"context"
	"testing"

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
