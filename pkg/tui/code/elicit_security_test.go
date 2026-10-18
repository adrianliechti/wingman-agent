package code

import (
	"testing"
	"time"
)

func TestConfirmationOffersOnlySingleActionChoices(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = app.Confirm(t.Context(), "Approve this action?") }()
	select {
	case show := <-app.queue:
		show()
	case <-time.After(time.Second):
		t.Fatal("approval did not open")
	}
	if app.popup == nil {
		t.Fatal("no approval popup")
	}
	if _, ok := app.popup.hotkeys['a']; ok {
		t.Fatal("blanket approval remains available")
	}
	app.dismissPrompt()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval did not cancel")
	}
}
