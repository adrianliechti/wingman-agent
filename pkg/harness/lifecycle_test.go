package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

func TestOperationReservationIncludesPreparation(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "close"}[closeSession], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			entered := make(chan struct{})
			a := &Agent{Config: &Config{PrepareTurn: func(ctx context.Context, _ []Content) ([]Content, error) {
				close(entered)
				<-ctx.Done()
				return nil, context.Cause(ctx)
			}}}
			finished := make(chan error, 1)
			go func() {
				_, err := a.Send(ctx, []Content{{Text: "first"}})
				finished <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("preparation was not reached")
			}
			if !a.Running() || a.QueueInput([]Content{{Text: "too early"}}) {
				t.Fatal("preparation must reserve the session without accepting steering")
			}
			if _, err := a.Send(ctx, []Content{{Text: "overlap"}}); !errors.Is(err, ErrTurnInProgress) {
				t.Fatalf("overlapping send: %v", err)
			}
			if _, err := a.Compact(ctx, ""); !errors.Is(err, ErrTurnInProgress) {
				t.Fatalf("overlapping compaction: %v", err)
			}
			if err := a.UndoTurn(ctx, "turn", nil); !errors.Is(err, ErrTurnInProgress) {
				t.Fatalf("overlapping undo: %v", err)
			}
			if err := a.RunExclusive(ctx, func(context.Context) error { t.Error("overlapping callback ran"); return nil }); !errors.Is(err, ErrTurnInProgress) {
				t.Fatalf("overlapping host operation: %v", err)
			}
			cause := errors.New("user interrupted preparation")
			if closeSession {
				cause = context.Canceled
				if err := a.Close(ctx, "other"); err != nil {
					t.Fatal(err)
				}
			} else {
				a.Cancel(cause)
			}
			if err := <-finished; !errors.Is(err, cause) {
				t.Fatalf("preparation error = %v, want %v", err, cause)
			}
			if a.Running() {
				t.Fatal("reservation survived failed preparation")
			}
			err := a.RunExclusive(ctx, func(context.Context) error { return nil })
			if closeSession && !errors.Is(err, ErrClosed) || !closeSession && err != nil {
				t.Fatalf("next operation: %v", err)
			}
		})
	}
}

func TestTurnReservationIncludesFinalization(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	a := &Agent{Config: &Config{Hooks: hook.Hooks{Interrupt: []hook.Interrupt{func(context.Context) {
		close(entered)
		<-release
	}}}}}
	stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Cancel(ErrInterrupted)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range stream {
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("finalization was not reached")
	}
	if _, err := a.Send(t.Context(), []Content{{Text: "overlap"}}); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("turn overtook finalization: %v", err)
	}
	if a.QueueInput([]Content{{Text: "too late"}}) {
		t.Fatal("finalization accepted steering")
	}
	unblock()
	<-finished
	if err := a.RunExclusive(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationFailureReleasesBudgetAndReservation(t *testing.T) {
	for _, panicSetup := range []bool{false, true} {
		var setupContext context.Context
		a := &Agent{Config: &Config{
			MaxTaskDuration: time.Minute,
			PrepareTurn: func(ctx context.Context, _ []Content) ([]Content, error) {
				setupContext = ctx
				if panicSetup {
					panic("preparation failed")
				}
				return nil, errors.New("preparation failed")
			},
		}}
		func() {
			defer func() {
				if r := recover(); (r != nil) != panicSetup {
					t.Errorf("panic = %v", r)
				}
			}()
			if _, err := a.Send(t.Context(), []Content{{Text: "start"}}); err == nil {
				t.Error("failed preparation succeeded")
			}
		}()
		if setupContext == nil || setupContext.Err() == nil || a.Running() {
			t.Fatal("preparation leaked its budget or reservation")
		}
	}
}

func TestCloseRunsCleanupAndEndHooksOnce(t *testing.T) {
	var calls []string
	cleanupErr := errors.New("cleanup failed")
	a := &Agent{Config: &Config{
		CloseSession: func() error { calls = append(calls, "cleanup"); return cleanupErr },
		Hooks: hook.Hooks{SessionEnd: []hook.SessionEnd{func(ctx context.Context, reason string) {
			calls = append(calls, reason)
			if hook.RuntimeFromContext(ctx).SessionID != "session" {
				t.Error("session context was lost")
			}
		}}},
	}}
	ctx := hook.WithRuntime(t.Context(), hook.Runtime{SessionID: "session"})
	if err := a.Close(ctx, "deleted"); !errors.Is(err, cleanupErr) {
		t.Fatal(err)
	}
	if err := a.Close(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "cleanup,deleted" {
		t.Fatalf("close callbacks = %v", calls)
	}
	if _, err := a.Send(ctx, []Content{{Text: "closed"}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close = %v", err)
	}
}

func TestConsumedStreamCannotAffectLaterOperation(t *testing.T) {
	a := &Agent{Config: &Config{}}
	stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Cancel(nil)
	for range stream {
	}
	before := a.StateSnapshot().Revision
	if err := a.RunExclusive(t.Context(), func(context.Context) error {
		var duplicateErr error
		for _, err := range stream {
			duplicateErr = err
		}
		if duplicateErr == nil || !a.Running() || a.StateSnapshot().Revision != before {
			t.Fatal("reusing an old stream affected the new operation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForSynchronousFinalizationBeforeCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	entered, canceled, release, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	a := &Agent{Config: &Config{CloseSession: func() error { close(cleaned); return nil }}}
	operationDone := make(chan error, 1)
	go func() {
		operationDone <- a.RunExclusive(ctx, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release // Final state must be saved before host resources close.
			return nil
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- a.Close(t.Context(), "other") }()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("close did not cancel the operation")
	}
	select {
	case <-cleaned:
		t.Fatal("close tore down resources before the operation settled")
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	if err := <-operationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("close did not clean up after finalization")
	}
}

func TestStartupPanicSettlesRecordedTurn(t *testing.T) {
	for _, phase := range []string{"session start", "user prompt"} {
		t.Run(phase, func(t *testing.T) {
			panicHook := func(context.Context, string) (hook.Outcome, error) { panic("startup callback failed") }
			var hooks hook.Hooks
			if phase == "session start" {
				hooks.SessionStart = []hook.SessionStart{panicHook}
			} else {
				hooks.UserPromptSubmit = []hook.UserPromptSubmit{panicHook}
			}
			a := &Agent{Config: &Config{Hooks: hooks}}
			func() {
				defer func() {
					if r := recover(); r != "startup callback failed" {
						t.Errorf("panic = %v", r)
					}
				}()
				_, _ = a.Send(t.Context(), []Content{{Text: "work"}})
			}()
			reviews := a.TurnReviews()
			if a.Running() || a.ActiveTurnID() != "" || len(reviews) != 1 || reviews[0].Outcome != string(RuntimeFailed) {
				t.Fatalf("startup panic left an unsettled turn: %+v, running=%v", reviews, a.Running())
			}
			terminals := 0
			for _, event := range a.StateSnapshot().Events {
				if event.Type == EventTurnTerminal {
					terminals++
					if !strings.Contains(event.Terminal.Error, "startup callback failed") {
						t.Errorf("terminal = %+v", event.Terminal)
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal events = %d", terminals)
			}
		})
	}
}
