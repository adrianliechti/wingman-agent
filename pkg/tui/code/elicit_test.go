package code

import (
	"context"
	"errors"
	"testing"
	"time"

	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestFreeTextAnswerPreservesTurnPhase(t *testing.T) {
	for _, phase := range []AppPhase{PhaseIdle, PhaseStopping, PhaseToolRunning} {
		app, _ := newQueueTestApp(t, 0)
		app.setPhase(phase)
		app.askActive = true
		app.askResponse = make(chan string, 1)
		app.editor.SetText("the answer")
		app.answerPrompt()
		if app.getPhase() != phase {
			t.Errorf("answer changed phase from %v to %v", phase, app.getPhase())
		}
	}
}

func TestSessionSwitchCancelsQuestionWithoutTakingTheNewDraft(t *testing.T) {
	for _, beforeDisplay := range []bool{false, true} {
		t.Run(map[bool]string{false: "displayed", true: "queued"}[beforeDisplay], func(t *testing.T) {
			app, _ := newQueueTestApp(t, 0)
			app.editor.SetText("first draft")
			app.drafts.Set("second", composerState{Text: "second draft"})
			done := make(chan error, 1)
			go func() {
				_, err := app.askLineLocked(t.Context(), "Which target?", "answer")
				done <- err
			}()
			var display func()
			select {
			case display = <-app.queue:
			case <-time.After(time.Second):
				t.Fatal("question never queued")
			}
			if !beforeDisplay {
				display()
				app.editor.SetText("partial answer")
			}
			app.activateSession("second")
			if beforeDisplay {
				display()
			}
			if app.askActive || app.editor.Text() != "second draft" {
				t.Fatal("old question took the new session's composer")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("question error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("old question did not stop")
			}
			(<-app.queue)()
			if app.editor.Text() != "second draft" {
				t.Fatal("old question cleanup replaced the new draft")
			}
			app.activateSession("session")
			if app.editor.Text() != "first draft" {
				t.Fatal("question lost the original session draft")
			}
		})
	}
}

func TestSessionSwitchCancelsApprovalWithoutClosingANewPicker(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	done := make(chan error, 1)
	go func() {
		_, err := app.askOptionsLocked(t.Context(), "Approval", "Continue?", []PopupItem{{ID: "yes"}}, false, nil)
		done <- err
	}()
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("approval never opened")
	}
	app.activateSession("second")
	if app.promptActive || app.popup != nil {
		t.Fatal("approval leaked into the new session")
	}
	picker := newPopup(popupList, "Model", nil, nil)
	app.popup = picker
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("approval error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not stop")
	}
	(<-app.queue)()
	if app.popup != picker {
		t.Fatal("old approval cleanup closed the new picker")
	}
}

func TestQuestionsFromAnotherSessionDoNotOpen(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	app.editor.SetText("current draft")
	done := make(chan error, 1)
	go func() {
		_, err := app.askLineLocked(corecode.WithSessionID(t.Context(), "background"), "Other session's question", "answer")
		done <- err
	}()
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("question never queued")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("question error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("question did not stop")
	}
	(<-app.queue)()
	if app.askActive || app.editor.Text() != "current draft" {
		t.Fatal("another session's question took the composer")
	}
}

func TestPromptReceiptPreservesTurnPhase(t *testing.T) {
	for _, phase := range []AppPhase{PhaseIdle, PhaseStopping, PhaseToolRunning} {
		app, _ := newStreamTestApp(nil)
		app.setPhase(phase)
		app.recordPrompt(t.Context(), "Approval", "Run the command?", "Yes")
		(<-app.queue)()
		if app.getPhase() != phase {
			t.Errorf("prompt receipt changed phase from %v to %v", phase, app.getPhase())
		}
	}
}

func TestPromptReceiptDoesNotCrossSessionActivations(t *testing.T) {
	for _, nextSession := range []string{"second", "session"} {
		t.Run(nextSession, func(t *testing.T) {
			app, _ := newQueueTestApp(t, 0)
			app.recordPrompt(t.Context(), "Approval", "Old question", "Yes")
			app.activateSession(nextSession)
			(<-app.queue)()
			if len(app.chat) != 0 {
				t.Fatal("old prompt receipt appeared in a new session activation")
			}
		})
	}
}

func TestPromptReceiptFromAnotherSessionIsIgnored(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	app.recordPrompt(corecode.WithSessionID(t.Context(), "other"), "Approval", "Old question", "Yes")
	(<-app.queue)()
	if len(app.chat) != 0 {
		t.Fatal("another session's prompt receipt appeared in the chat")
	}
}

func TestPromptCleanupKeepsAnUnrelatedPicker(t *testing.T) {
	app, _ := newStreamTestApp(nil)
	done := make(chan struct{})
	go func() {
		_, _ = app.askOptionsLocked(t.Context(), "Approval", "Continue?", []PopupItem{{ID: "yes", Label: "Yes"}}, false, nil)
		close(done)
	}()
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("prompt did not open")
	}
	app.popup.onAccept([]string{"yes"})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish")
	}
	picker := newPopup(popupList, "Model", nil, nil)
	app.popup = picker
	(<-app.queue)()
	if app.popup != picker {
		t.Fatal("old prompt cleanup closed the new picker")
	}
}
