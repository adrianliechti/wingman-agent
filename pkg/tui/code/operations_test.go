package code

import (
	"context"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestSessionOperationAllowsInputAndCanBeCancelled(t *testing.T) {
	app, _ := newStreamTestApp(nil)
	app.ctx = t.Context()
	app.editor = NewEditor()
	entered := make(chan struct{})
	app.runSessionOperation("settings", func(ctx context.Context, _ string) error { close(entered); <-ctx.Done(); return ctx.Err() }, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	app.handleKey(inline.KeyEvent{Key: inline.KeyRune, Rune: 'a'})
	if app.editor.Text() != "a" {
		t.Fatal("backend operation blocked input")
	}
	app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	waitForSessionOperation(t, app)
	if app.editor.Text() != "a" {
		t.Fatal("cancelling settings discarded draft")
	}
}

func TestSettingsOperationCanAskForApprovalOnUIThread(t *testing.T) {
	app, _ := newStreamTestApp(nil)
	app.ctx = t.Context()
	app.editor = NewEditor()
	finished := false
	app.runSessionOperation("settings", func(ctx context.Context, _ string) error {
		_, err := app.Confirm(ctx, "approve test settings")
		return err
	}, func() { finished = true })
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for app.popup == nil {
		select {
		case fn := <-app.queue:
			fn()
		case <-deadline.C:
			t.Fatal("approval deadlocked behind settings RPC")
		}
	}
	app.handlePopupKey(inline.KeyEvent{Key: inline.KeyRune, Rune: 'y'})
	waitForSessionOperation(t, app)
	if !finished {
		t.Fatal("approved operation did not finish")
	}
	if app.getPhase() != PhaseIdle {
		t.Fatalf("settings approval left an idle session running: %v", app.getPhase())
	}
}
