package code

import (
	"context"
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func drainStateUpdates(a *App) {
	for {
		select {
		case update := <-a.queue:
			update()
		case <-a.renderRequests:
			a.applyPendingPhase()
			a.refreshMetadata()
			a.invalidate()
		default:
			return
		}
	}
}

func TestRenderRequestsCoalesceAndThrottleOnUILoop(t *testing.T) {
	testenv.WingmanHome(t)
	testenv.UserHome(t)
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		coder := newUITestAgent(nil)
		coder.workspace.RootPath = dir
		a := New(t.Context(), coder, "session")
		a.WithTerminal(inline.NewTerminal(inline.WithIO(strings.NewReader(""), io.Discard, func() (int, int) {
			return 100, 24
		})))
		done := make(chan error, 1)
		go func() { done <- a.Run() }()
		defer func() {
			a.stop()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()

		a.withCurrentSession("session", func() { a.queuePhase(PhaseThinking) })
		synctest.Wait()
		if a.getPhase() != PhaseThinking {
			t.Fatal("first render request was delayed")
		}
		a.withCurrentSession("session", func() { a.queuePhase(PhaseToolRunning) })
		var producers sync.WaitGroup
		for range 100 {
			producers.Go(a.requestRender)
		}
		producers.Wait()
		synctest.Wait()
		if a.getPhase() != PhaseThinking || len(a.renderRequests) != 1 {
			t.Fatal("requests were not coalesced during the render cooldown")
		}
		time.Sleep(renderInterval - time.Nanosecond)
		synctest.Wait()
		if a.getPhase() != PhaseThinking {
			t.Fatal("render request bypassed the frame limit")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if a.getPhase() != PhaseToolRunning || len(a.renderRequests) != 0 {
			t.Fatal("buffered request was not applied after the cooldown")
		}
		// Quit while another request is waiting on the timer. Shutdown must
		// not need to wait for or drain a background render callback.
		a.requestRender()
	})
}

func TestStoppingPhaseSurvivesLateUpdatesUntilIdle(t *testing.T) {
	a := &App{sessionEpoch: 1}
	a.setPhase(PhaseStopping)
	for _, phase := range []AppPhase{PhaseThinking, PhaseStreaming, PhaseToolRunning, PhaseRetrying} {
		a.pendingPhase.Store(&phaseUpdate{epoch: 1, phase: phase})
		a.applyPendingPhase()
		if a.getPhase() != PhaseStopping {
			t.Fatalf("late %v update cleared stopping state", phase)
		}
	}
	a.setPhase(PhaseIdle)
	a.setPhase(PhaseThinking)
	if a.getPhase() != PhaseThinking {
		t.Fatal("stopping state leaked into the next turn")
	}
}

type scheduleCountTestAgent struct {
	*uiTestAgent
	store schedule.Store
}

func (a *scheduleCountTestAgent) Schedules(string) schedule.Store { return a.store }

type scheduleCountTestStore struct {
	schedule.Store
	list func() ([]schedule.Task, error)
}

func (s scheduleCountTestStore) List() ([]schedule.Task, error) { return s.list() }

func TestScheduleCountCompletionRunsOnUILoop(t *testing.T) {
	for _, outcome := range []string{"success", "error", "session changed"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, coder := newStreamTestApp(nil)
				calls := 0
				a.agent = &scheduleCountTestAgent{uiTestAgent: coder, store: scheduleCountTestStore{list: func() ([]schedule.Task, error) {
					calls++
					if outcome == "error" {
						return nil, errors.New("unavailable")
					}
					return []schedule.Task{{Status: schedule.StatusActive}}, nil
				}}}
				a.activeScheduleCount()
				synctest.Wait() // Completion is waiting in the UI queue.
				if outcome == "session changed" {
					a.sessionID = "other"
					a.sessionEpoch++
				}
				time.Sleep(2 * time.Second)
				a.activeScheduleCount()
				synctest.Wait()
				if calls != 1 || !a.scheduleCountPending || a.scheduleCount != 0 {
					t.Fatalf("completion escaped UI queue: calls=%d pending=%t count=%d", calls, a.scheduleCountPending, a.scheduleCount)
				}
				drainStateUpdates(a)
				want := 0
				if outcome == "success" {
					want = 1
				}
				if a.scheduleCountPending || a.scheduleCount != want {
					t.Fatalf("completion pending=%t count=%d, want %d", a.scheduleCountPending, a.scheduleCount, want)
				}
				a.activeScheduleCount()
				synctest.Wait()
				drainStateUpdates(a)
				if calls != 2 {
					t.Fatal("refresh did not become available after completion")
				}
			})
		})
	}
}

func TestSessionSwitchDiscardsPendingTurnUpdatesAndComposerContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _ := newStreamTestApp(nil)
		a.editor = NewEditor()
		a.editor.SetText("old draft")
		a.pendingFiles = []string{"old.go"}
		a.pendingContent = []harness.Content{{Text: "old attachment"}}
		a.pendingEcho = []pendingEchoItem{{ID: "old-input", Text: "queued"}}
		message := harness.Message{Content: []harness.Content{{Text: "old output"}}}
		a.handleTurnEvent(corecode.TurnEvent{SessionID: "session", Message: &message})
		a.onSessionUpdate("session")
		synctest.Wait() // Leave the old repaint waiting in the UI queue.
		a.activateSession("other")
		drainStateUpdates(a)
		if a.getPhase() != PhaseIdle || len(a.snapshotStreamState()) != 0 {
			t.Fatal("queued update restored the previous session's turn")
		}
		if a.editor.Text() != "" || len(a.pendingFiles)+len(a.pendingContent)+len(a.pendingEcho) != 0 {
			t.Fatal("previous session's composer or queued input survived activation")
		}
	})
}

func TestFinishingTurnDoesNotClearTheNextTurnsOutput(t *testing.T) {
	testenv.WingmanHome(t)
	for _, state := range []corecode.TurnInputState{corecode.TurnInputCompleted, corecode.TurnInputFailed, corecode.TurnInputCancelled, corecode.TurnInputIncomplete} {
		t.Run(string(state), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, coder := newStreamTestApp(nil)
				a.turns = corecode.NewTurnManager(a.ctx, coder, a.handleTurnEvent)
				defer a.turns.Close()
				a.termFocused = true
				previous := harness.Message{Content: []harness.Content{{Text: "previous output"}}}
				a.handleTurnEvent(corecode.TurnEvent{SessionID: "session", Message: &previous})
				a.handleTurnEvent(corecode.TurnEvent{SessionID: "session", State: state, Executed: true, Err: errors.New("test failure")})
				// TurnManager emits the next Active and output after the terminal
				// callback returns, potentially before the UI processes that callback.
				a.handleTurnEvent(corecode.TurnEvent{SessionID: "session", State: corecode.TurnInputActive})
				next := harness.Message{Content: []harness.Content{{Text: "next output"}}}
				a.handleTurnEvent(corecode.TurnEvent{SessionID: "session", Message: &next})
				synctest.Wait()
				drainStateUpdates(a)
				output := ansi.Strip(strings.Join(a.streamCells(100), "\n"))
				if !strings.Contains(output, "next output") || strings.Contains(output, "previous output") {
					t.Fatalf("completion cleared the wrong turn: %q", output)
				}
				if a.getPhase() != PhaseStreaming {
					t.Fatalf("completion replaced the new phase with %v", a.getPhase())
				}
				if a.turnStart.IsZero() {
					t.Fatal("completion reset the new turn's clock")
				}
			})
		})
	}
}

type metadataTestAgent struct {
	*uiTestAgent
	handler func(string)
	usage   harness.Usage
}

func (a *metadataTestAgent) SetSessionUpdateHandler(handler func(string)) { a.handler = handler }
func (a *metadataTestAgent) Usage(string) harness.Usage                   { return a.usage }

func TestIdleSessionMetadataRefreshesWithoutInputAndCoalesces(t *testing.T) {
	testenv.WingmanHome(t)
	testenv.UserHome(t)
	workspace := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		coder := &metadataTestAgent{uiTestAgent: newUITestAgent(nil)}
		coder.workspace.RootPath = workspace
		a := New(context.Background(), coder, "session")
		defer a.turns.Close()
		if coder.handler == nil {
			t.Fatal("TUI did not subscribe to session metadata")
		}
		a.showCommandCenter()
		a.popup.SetQuery("plan")
		a.dirty = false
		coder.mode = "plan"
		coder.usage = harness.Usage{InputTokens: 42, ContextWindow: 1000}
		// A settings command can notify on the UI thread, even when its queue
		// is full. Repeated notifications must not wait for that queue to drain.
		for range cap(a.queue) {
			a.queue <- func() {}
		}
		for range 100 {
			coder.handler("session")
		}
		drainStateUpdates(a)
		synctest.Wait()
		drainStateUpdates(a)
		if !a.dirty || a.inputTokens != 42 || a.contextWindow != 1000 || a.getPhase() != PhaseIdle {
			t.Fatal("idle metadata did not repaint with fresh usage")
		}
		if item := findPopupItem(a.popup, "builtin:/plan"); item == nil || !item.Checked || a.popup.query != "plan" {
			t.Fatal("open command center did not refresh its selected mode")
		}
		a.dirty = false
		coder.handler("other")
		synctest.Wait()
		drainStateUpdates(a)
		if a.dirty {
			t.Fatal("another session's metadata changed the current UI")
		}
	})
}

func TestRejectedTurnKeepsComposerAndAttachments(t *testing.T) {
	testenv.WingmanHome(t)
	testenv.UserHome(t)
	a, coder := newStreamTestApp(nil)
	coder.workspace.RootPath = t.TempDir()
	a.editor = NewEditor()
	a.editor.SetText("keep this draft")
	a.pendingFiles = []string{"main.go"}
	content := []harness.Content{{Text: "attached content"}}
	a.pendingContent = content
	a.turns = corecode.NewTurnManager(a.ctx, coder, a.handleTurnEvent)
	a.turns.Close()
	a.submitInput()
	if a.editor.Text() != "keep this draft" || !reflect.DeepEqual(a.pendingFiles, []string{"main.go"}) || !reflect.DeepEqual(a.pendingContent, content) {
		t.Fatal("rejected submission discarded composer content")
	}
	if len(a.pendingEcho) != 0 || len(a.snapshotStreamState()) != 0 {
		t.Fatal("rejected submission left a phantom message")
	}
}

type streamingTestAgent struct{ *uiTestAgent }

func (a *streamingTestAgent) Send(ctx context.Context, _ string, _ []harness.Content) (iter.Seq2[harness.Message, error], error) {
	return func(yield func(harness.Message, error) bool) {
		yield(harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "streamed response"}}}, nil)
		<-ctx.Done()
	}, nil
}

func TestAcceptedUserInputPrecedesImmediateStreamOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, base := newStreamTestApp(nil)
		coder := &streamingTestAgent{uiTestAgent: base}
		a.agent = coder
		a.turns = corecode.NewTurnManager(a.ctx, coder, a.handleTurnEvent)
		defer a.turns.Close()
		if !a.submitAgentInput([]harness.Content{{Text: "user input"}}, "user input") {
			t.Fatal("submission failed")
		}
		synctest.Wait()
		view := ansi.Strip(strings.Join(a.chatViewLines(100), "\n"))
		user, answer := strings.Index(view, "user input"), strings.Index(view, "streamed response")
		if user < 0 || answer <= user || strings.Contains(view, "(queued)") {
			t.Fatalf("user input was not promoted before the response: %q", view)
		}
	})
}

func TestToolProgressIgnoresOtherSessions(t *testing.T) {
	a, _ := newStreamTestApp(nil)
	a.streamCurrent = streamSnapshot{toolID: "call", toolName: "shell"}
	a.onToolProgress(corecode.WithSessionID(context.Background(), "other"), "call", "wrong session")
	if a.streamCurrent.toolProgress != "" {
		t.Fatal("another session changed the visible tool")
	}
}
